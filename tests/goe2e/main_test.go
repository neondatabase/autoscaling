package goe2e

import (
	"context"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test struct with apply and poll methods
type Test struct {
	t         *testing.T
	namespace string
	timeout   time.Duration
	scheme    *runtime.Scheme
}

// NewTest creates a new test instance
func NewTest(t *testing.T) *Test {
	// Create scheme and register VirtualMachine types
	testScheme := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(testScheme))
	require.NoError(t, vmv1.AddToScheme(testScheme))

	namespace := fmt.Sprintf("goe2e-%s-%s-%d-test", t.Name(), time.Now().Format("2006-01-02-15-04-05"), rand.Intn(1000000))
	namespace = strings.ToLower(namespace)
	tt := &Test{
		t:         t,
		namespace: namespace,
		timeout:   60 * time.Second,
		scheme:    testScheme,
	}
	tt.kubectl([]string{"create", "namespace", namespace}, "")
	return tt
}

func (t *Test) kubectl(args []string, in string) []byte {
	cmd := exec.Command("kubectl", args...)
	t.t.Logf("Running %s", cmd)
	if in != "" {
		cmd.Stdin = strings.NewReader(in)
	}
	output, err := cmd.CombinedOutput()
	require.NoError(t.t, err, "%s failed: %s", cmd, string(output))

	return output
}

func (test *Test) applyRaw(yamlContent string) {
	fmt.Println(yamlContent)
	test.kubectl([]string{"apply", "-f", "-"}, yamlContent)
}

func (test *Test) apply(obj runtime.Object) {
	// Use meta.Accessor to set namespace if possible
	accessor, err := meta.Accessor(obj)
	require.NoError(test.t, err)
	fmt.Println("accessor", accessor.GetNamespace())
	if accessor.GetNamespace() == "" {
		accessor.SetNamespace(test.namespace)
	}

	// Auto-fill APIVersion and Kind using scheme
	gvks, _, err := test.scheme.ObjectKinds(obj)
	require.NoError(test.t, err)
	require.NotEmpty(test.t, gvks, "No GVKs found for object type")

	// Set the GVK on the object
	obj.GetObjectKind().SetGroupVersionKind(gvks[0])

	// Encode and apply
	data, err := yaml.Marshal(obj)
	require.NoError(test.t, err)

	test.applyRaw(string(data))
}

func (test *Test) get(k8sType string, name types.NamespacedName) *unstructured.Unstructured {
	args := formatArgs(name, "get", k8sType, "-o", "json")
	output := test.kubectl(args, "")

	obj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, output)
	require.NoError(test.t, err)

	return obj.(*unstructured.Unstructured)
}

func (t *Test) toVM(obj *unstructured.Unstructured) *vmv1.VirtualMachine {
	return convertToTyped[*vmv1.VirtualMachine](t.t, obj)
}

func convertToTyped[T runtime.Object](t *testing.T, obj *unstructured.Unstructured) T {
	var target T
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &target)
	require.NoError(t, err, "Failed to convert unstructured to typed object")
	return target
}

func formatArgs(name types.NamespacedName, args ...string) []string {
	res := append(args, name.Name)
	if name.Namespace != "" {
		res = append(res, "-n", name.Namespace)
	}
	return res
}

func (test *Test) poll(
	ctx context.Context,
	k8sType string,
	name types.NamespacedName,
	condition func(obj runtime.Object) bool,
) *unstructured.Unstructured {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			test.t.Fatalf("Context cancelled while polling %s", name)
			return nil
		case <-ticker.C:
			obj := test.get(k8sType, name)
			if condition == nil || condition(obj) {
				return obj
			}
		}
	}
}

// delete removes a resource using kubectl
func (test *Test) delete(k8sType string, name types.NamespacedName) error {
	test.t.Logf("Deleting %s/%s", k8sType, name)

	args := formatArgs(name, "delete", k8sType, "--ignore-not-found=true")

	output := test.kubectl(args, "")
	test.t.Logf("Deleted successfully: %s", string(output))
	return nil
}

func (test *Test) finish() {
	test.kubectl([]string{"delete", "namespace", test.namespace}, "")
}

func TestVM(t *testing.T) {
	test := NewTest(t)
	defer test.finish()

	vm := &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-vm",
		},
		Spec: vmv1.VirtualMachineSpec{
			SchedulerName: "autoscale-scheduler",
			Guest: vmv1.Guest{
				CPUs: vmv1.CPUs{
					Min: vmv1.MilliCPU(250),
					Use: vmv1.MilliCPU(250),
					Max: vmv1.MilliCPU(250),
				},
				MemorySlotSize: resource.MustParse("1Gi"),
				MemorySlots: vmv1.MemorySlots{
					Min: 1,
					Use: 1,
					Max: 1,
				},
				RootDisk: vmv1.RootDisk{
					Image: "vm-postgres:15-bullseye",
					Size:  resource.MustParse("1Gi"),
				},
			},
			CpuScalingMode: lo.ToPtr(vmv1.CpuScalingModeSysfs),
			RestartPolicy:  vmv1.RestartPolicyAlways,
		},
	}
	test.apply(vm)

	obj := test.poll(context.Background(), "neonvm", types.NamespacedName{Name: "test-vm", Namespace: test.namespace}, nil)

	vmGet := test.toVM(obj)

	require.Equal(t, vm.Name, vmGet.Name)
	require.Equal(t, test.namespace, vmGet.Namespace)
}
