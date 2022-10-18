package agent

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
)

// Args stores the static configuration data assigned provided to the autoscaling agent by its
// environment
//
// Most of the values are expected to be roughly the same across VM instances, but things like
// InitVCPU may differ per-VM, depending on how it's been configured by the user.
type Args struct {
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

	// InitVCPU is the initial number of vCPUs assigned to the VM. It is taken from the INIT_VCPU
	// environment variable.
	//
	// We expect that InitVCPU will match the configured amount for Virtink's
	// vm.spec.instance.cpu.sockets, and WILL NOT adjust the initially assigned amount to match.
	//
	// We require that MinVCPU <= InitVCPU <= MaxVCPU.
	InitVCPU uint16
	// MinVCPU is the minimum number of vCPUs that the VM may be assigned. It is taken from the
	// MIN_VCPU environment variable.
	//
	// We require that MinVCPU <= InitVCPU <= MaxVCPU.
	MinVCPU uint16
	// MaxVCPU is the maximum number of vCPUs that the VM may be assigned. It is taken from the
	// MAX_VCPU environment variable.
	//
	// We require that MinVCPU <= InitVCPU <= MaxVCPU.
	MaxVCPU uint16

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

func getEnvVar[T any](err *error, varName string, parse func(string) (T, error)) (v T) {
	if *err != nil {
		return
	}

	s := os.Getenv(varName)
	if s == "" {
		*err = fmt.Errorf("Missing %s in environment", varName)
	} else if v, *err = parse(s); *err != nil {
		*err = fmt.Errorf("Bad value for %s: %s", varName, *err)
	}

	return
}

func ArgsFromEnv() (Args, error) {
	var err error

	// Helper identity function for strings
	parseString := func(s string) (string, error) { return s, nil }
	// Helper decimal integer parsing function
	parseDecimalUint16 := func(s string) (uint16, error) {
		u, err := strconv.ParseUint(s, 10, 16)
		return uint16(u), err
	}
	parseVCPU := func(s string) (uint16, error) {
		n, err := parseDecimalUint16(s)
		if err == nil && n == 0 {
			err = fmt.Errorf("vCPU amount must be > 0")
		}
		return n, err
	}

	args := Args{
		ConfigPath:      getEnvVar(&err, "CONFIG_PATH", parseString),
		K8sPodName:      getEnvVar(&err, "K8S_POD_NAME", parseString),
		K8sPodNamespace: getEnvVar(&err, "K8S_POD_NAMESPACE", parseString),
		MetricsURL:      getEnvVar(&err, "METRICS_URL", url.Parse),
		InitVCPU:        getEnvVar(&err, "INIT_VCPU", parseVCPU),
		MinVCPU:         getEnvVar(&err, "MIN_VCPU", parseVCPU),
		MaxVCPU:         getEnvVar(&err, "MAX_VCPU", parseVCPU),
		ReadinessPort:   getEnvVar(&err, "READINESS_PORT", parseDecimalUint16),
		PoliteExitPort:  getEnvVar(&err, "POLITE_EXIT_PORT", parseDecimalUint16),
	}

	if err != nil {
		return Args{}, err
	} else if !(args.MinVCPU <= args.InitVCPU && args.InitVCPU <= args.MaxVCPU) {
		return Args{}, fmt.Errorf("Invalid vCPU parameters: must have MIN_VCPU <= INIT_VCPU <= MAX_VCPU")
	} else {
		return args, err
	}
}
