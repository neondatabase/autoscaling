package agent

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	vmapi "github.com/neondatabase/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/neonvm/client/clientset/versioned"

	"github.com/neondatabase/autoscaling/pkg/api"
)

// Args encapsulates the arguments from both EnvArgs and PodArgs
type Args struct {
	EnvArgs
	PodArgs
	VmInfo *api.VmInfo
}

// EnvArgs stores the static configuration data assigned to the autoscaling agent by its
// environment
//
// Most of the values are expected to be roughly the same across VM instances, but things like
// InitVCPU may differ per-VM, depending on how it's been configured by the user.
type EnvArgs struct {
	// ConfigPath gives the path to read static configuration from. It is taken from the CONFIG_PATH
	// environment variable.
	ConfigPath string

	// K8sPodName is the Kubernetes in-cluster name of the pod containing this instance of the
	// autoscaler agent. It is taken from the K8S_POD_NAME environment variable.
	K8sPodName string
	// K8sPodNamespace is the Kubernetes namespace of the pod containing this instance of the
	// autoscaler agent. It is taken from the K8S_POD_NAMESPACE environment variable.
	K8sPodNamespace string

	// MetricsURL gives a static URL we can use to query the VM's metrics. It is taken from the
	// METRICS_URL environment variable.
	//
	// We expect Prometheus' node_exporter to be listening on the URL.
	MetricsURL *url.URL

	// ReadinessPort gives a port number to respond to /healthz probes on once we're ready. It is
	// taken from the READINESS_PORT environment variable
	//
	// If not provided, no readiness server will be spawned.
	ReadinessPort uint16
	// PoliteExitPort gives a port we should listen on to receive polite exit requests from the
	// Kubernetes controller. It is taken from the POLITE_EXIT_PORT environment variable.
	//
	// If not provided, no server will be spawned to listen for such requests.
	PoliteExitPort uint16
}

func getEnvVar[T any](
	err *error,
	require_nonempty bool,
	varName string,
	parse func(string) (T, error),
) (v T) {
	if *err != nil {
		return
	}

	s := os.Getenv(varName)
	if s == "" && require_nonempty {
		*err = fmt.Errorf("Missing %s in environment", varName)
	} else if v, *err = parse(s); *err != nil {
		*err = fmt.Errorf("Bad value for environment variable %s: %s", varName, *err)
	}

	return
}

func ArgsFromEnv() (EnvArgs, error) {
	var err error

	// Helper identity function for strings
	parseString := func(s string) (string, error) { return s, nil }
	// Helper decimal integer parsing function
	parseDecimalUint16 := func(s string) (uint16, error) {
		if s == "" {
			return 0, nil
		}
		u, err := strconv.ParseUint(s, 10, 16)
		return uint16(u), err
	}

	args := EnvArgs{
		ConfigPath:      getEnvVar(&err, true, "CONFIG_PATH", parseString),
		K8sPodName:      getEnvVar(&err, true, "K8S_POD_NAME", parseString),
		K8sPodNamespace: getEnvVar(&err, true, "K8S_POD_NAMESPACE", parseString),
		MetricsURL:      getEnvVar(&err, true, "METRICS_URL", url.Parse),
		ReadinessPort:   getEnvVar(&err, false, "READINESS_PORT", parseDecimalUint16),
		PoliteExitPort:  getEnvVar(&err, false, "POLITE_EXIT_PORT", parseDecimalUint16),
	}

	if err != nil {
		return EnvArgs{}, err
	} else {
		return args, err
	}
}

// PodArgs stores the static configuration data provided to the autoscaling agent by the metadata of
// the Kubernetes pod containing it
type PodArgs struct {
	// SchedulerName is the name of the scheduler that the pod requested, directly copied from
	// PodSpec.ScehdulerName
	//
	// We require that SchedulerName is not "" or "default-scheduler", because autoscaling requires
	// our custom scheduler.
	SchedulerName string

	// VmName is the name of the VM object running in this pod
	VmName string
}

func getPodLabel[T any](err *error, pod *corev1.Pod, labelName string, parse func(string) (T, error)) (v T) {
	if *err != nil {
		return
	}

	s, ok := pod.Labels[labelName]
	if !ok {
		*err = fmt.Errorf("Missing label %s in pod metadata", labelName)
	} else if v, *err = parse(s); *err != nil {
		*err = fmt.Errorf("Bad value for label %q in pod metadata: %s", labelName, *err)
	}

	return
}

func ArgsFromPod(ctx context.Context, client *kubernetes.Clientset, envArgs EnvArgs) (PodArgs, error) {
	pod, err := client.CoreV1().Pods(envArgs.K8sPodNamespace).Get(ctx, envArgs.K8sPodName, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("Error getting self pod %s:%s: %s", envArgs.K8sPodNamespace, envArgs.K8sPodName, err)
		return PodArgs{}, err
	}

	vmName, ok := pod.Labels[vmapi.VirtualMachineNameLabel]
	if !ok {
		return PodArgs{}, fmt.Errorf("Missing label %s in pod metadata", vmapi.VirtualMachineNameLabel)
	}

	if pod.Spec.SchedulerName == "" || pod.Spec.SchedulerName == "default-scheduler" {
		// default-scheduler is the name of the scheduler used for pods not assigned to a particular
		// scheduler.
		err = fmt.Errorf("Pod is not using a custom scheduler (SchedulerName = %q)", pod.Spec.SchedulerName)
		return PodArgs{}, err
	}

	return PodArgs{
		SchedulerName: pod.Spec.SchedulerName,
		VmName:        vmName,
	}, nil
}

func ArgsFromVM(ctx context.Context, client *vmclient.Clientset, envArgs EnvArgs, podArgs PodArgs) (*api.VmInfo, error) {
	vm, err := client.NeonvmV1().
		VirtualMachines(envArgs.K8sPodNamespace).
		Get(ctx, podArgs.VmName, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("Error getting self VM %s:%s: %w", envArgs.K8sPodNamespace, podArgs.VmName, err)
		return nil, err
	}

	vmInfo, err := api.ExtractVmInfo(vm)
	if err != nil {
		err = fmt.Errorf("Error extracting VmInfo from VM %s:%s: %w", envArgs.K8sPodNamespace, podArgs.VmName, err)
		return nil, err
	}

	return vmInfo, nil
}
