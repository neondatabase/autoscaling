package controllers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	certv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

// defaultVm returns a VM which is similar to what we can reasonably
// expect from the control plane.
func defaultVm() *vmv1.VirtualMachine {
	return &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm",
			Namespace: "default",
		},
		Spec: vmv1.VirtualMachineSpec{
			EnableSSH:          lo.ToPtr(false),
			EnableAcceleration: lo.ToPtr(true),
			//nolint:exhaustruct // This is a test
			Guest: vmv1.Guest{
				KernelImage:         lo.ToPtr("kernel-img"),
				AppendKernelCmdline: nil,
				CPUs: vmv1.CPUs{
					Min: vmv1.MilliCPU(1000),
					Max: vmv1.MilliCPU(2000),
					Use: vmv1.MilliCPU(1500),
				},
				MemorySlots: vmv1.MemorySlots{
					Min: int32(1),
					Max: int32(32),
					Use: int32(2),
				},
				MemorySlotSize: resource.MustParse("1Gi"),
			},
		},
		//nolint:exhaustruct // Intentionally left empty
		Status: vmv1.VirtualMachineStatus{},
	}
}

type testParams struct {
	t      *testing.T
	ctx    context.Context
	r      *VMReconciler
	client client.Client
	origVM *vmv1.VirtualMachine
}

var testReconcilerMetrics = MakeReconcilerMetrics()

func newTestParams(t *testing.T) *testParams {
	os.Setenv("VM_RUNNER_IMAGE", "vm-runner-img")

	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stdout),
		zap.Level(zapcore.DebugLevel))
	ctx := log.IntoContext(context.Background(), logger)

	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(vmv1.SchemeGroupVersion, &vmv1.VirtualMachine{})
	scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.Pod{})
	scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.Secret{})
	scheme.AddKnownTypes(certv1.SchemeGroupVersion, &certv1.CertificateRequest{})

	params := &testParams{
		t:   t,
		ctx: ctx,
		client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&vmv1.VirtualMachine{}).
			Build(),
		r:      nil,
		origVM: nil,
	}

	params.r = &VMReconciler{
		Client: params.client,
		Scheme: scheme,
		Config: &ReconcilerConfig{
			DisableRunnerCgroup:     false,
			MaxConcurrentReconciles: 10,
			SkipUpdateValidationFor: nil,
			QEMUDiskCacheSettings:   "",
			MemhpAutoMovableRatio:   "301",
			FailurePendingPeriod:    time.Minute,
			FailingRefreshInterval:  time.Minute,
			AtMostOnePod:            false,
			DefaultCPUScalingMode:   vmv1.CpuScalingModeQMP,
			NADConfig:               nil,
			UseVirtioConsole:        false,
		},
		Metrics: testReconcilerMetrics,
		IPAM:    nil,
	}

	return params
}

// initVM initializes the VM in the fake client and returns the VM
func (p *testParams) initVM(vm *vmv1.VirtualMachine) *vmv1.VirtualMachine {
	err := p.client.Create(p.ctx, vm)
	require.NoError(p.t, err)
	p.origVM = vm

	// Do serialize/deserialize, to normalize resource.Quantity
	return p.getVM()
}

func (p *testParams) getVM() *vmv1.VirtualMachine {
	var obj vmv1.VirtualMachine
	err := p.client.Get(p.ctx, client.ObjectKeyFromObject(p.origVM), &obj)
	require.NoError(p.t, err)

	return &obj
}

func TestReconcile(t *testing.T) {
	params := newTestParams(t)
	origVM := params.initVM(defaultVm())

	req := reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(origVM),
	}

	// Round 1
	res, err := params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)

	// Added finalizer
	assert.Equal(t, reconcile.Result{
		Requeue: true,
	}, res)
	assert.Contains(t, params.getVM().Finalizers, virtualmachineFinalizer)

	// Added default value for the cpuScalingMode
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, true, res.Requeue)

	// Round 2
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// VM is pending
	assert.Equal(t, vmv1.VmPending, params.getVM().Status.Phase)

	// Round 3
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// We now have a pod
	vm := params.getVM()
	assert.NotEmpty(t, vm.Status.PodName)
	// Spec is unchanged except cpuScalingMode and targetArchitecture
	var origWithModifiedFields vmv1.VirtualMachine
	origVM.DeepCopy().DeepCopyInto(&origWithModifiedFields)
	origWithModifiedFields.Spec.CpuScalingMode = lo.ToPtr(vmv1.CpuScalingModeQMP)
	assert.Equal(t, vm.Spec, origWithModifiedFields.Spec)

	// Round 4
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// Nothing is updating the pod status, so nothing changes in VM as well
	assert.Equal(t, vm, params.getVM())
}

func prettyPrint(t *testing.T, obj any) {
	t.Helper()
	s, _ := json.MarshalIndent(obj, "", "\t")
	t.Logf("%s\n", s)
}

func TestRunningPod(t *testing.T) {
	params := newTestParams(t)
	origVM := defaultVm()
	origVM.Finalizers = append(origVM.Finalizers, virtualmachineFinalizer)
	origVM.Status.Phase = vmv1.VmPending

	origVM = params.initVM(origVM)

	req := reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(origVM),
	}

	// Round 1
	// Requeue because we expect default values to be set
	res, err := params.r.Reconcile(params.ctx, req)
	require.NoError(t, err)
	assert.Equal(t, true, res.Requeue)

	res, err = params.r.Reconcile(params.ctx, req)
	require.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// We now have a pod
	podName := params.getVM().Status.PodName
	podKey := client.ObjectKey{
		Namespace: origVM.Namespace,
		Name:      podName,
	}
	var pod corev1.Pod
	err = params.client.Get(params.ctx, podKey, &pod)
	require.NoError(t, err)

	assert.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, "neonvm-runner", pod.Spec.Containers[0].Name)
	assert.Equal(t, "vm-runner-img", pod.Spec.Containers[0].Image)
	assert.Len(t, pod.Spec.InitContainers, 2)
	assert.Equal(t, "init", pod.Spec.InitContainers[0].Name)
	assert.Equal(t, "init-kernel", pod.Spec.InitContainers[1].Name)

	prettyPrint(t, pod)

	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:  "neonvm-runner",
		Ready: true,
	})
	err = params.client.Status().Update(params.ctx, &pod)
	require.NoError(t, err)
	prettyPrint(t, pod)
	// assert pod is ready
	assert.True(t, lo.ContainsBy(pod.Status.ContainerStatuses, func(c corev1.ContainerStatus) bool {
		return c.Name == "neonvm-runner" && c.Ready
	}))

	assert.Equal(t, runnerRunning, runnerStatus(&pod))

	// Round 2
	res, err = params.r.Reconcile(params.ctx, req)
	require.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	vm := params.getVM()

	// VM is now running
	assert.Equal(t, vmv1.VmRunning, vm.Status.Phase)
	assert.Len(t, vm.Status.Conditions, 1)
	assert.Equal(t, vm.Status.Conditions[0].Type, typeAvailableVirtualMachine)
}

func TestNodeAffinity(t *testing.T) {
	t.Run("no affinity", func(t *testing.T) {
		origVM := defaultVm()
		origVM.Spec.TargetArchitecture = lo.ToPtr(vmv1.CPUArchitectureAMD64)
		affinity := affinityForVirtualMachine(origVM)
		prettyPrint(t, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
		assert.Equal(t, 1, len(affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms))
		assert.Equal(t, "linux", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0])
		assert.Equal(t, "amd64", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[1].Values[0])
	})

	t.Run("affinity given without architecture", func(t *testing.T) {
		origVM := defaultVm()
		origVM.Spec.TargetArchitecture = lo.ToPtr(vmv1.CPUArchitectureAMD64)
		origVM.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: "topology.kubernetes.io/zone", Operator: "In", Values: []string{"zoneid"}},
							},
						},
					},
				},
			},
		}
		affinity := affinityForVirtualMachine(origVM)
		prettyPrint(t, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
		assert.Equal(t, 2, len(affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms))
		assert.Equal(t, "zoneid", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0])
		assert.Equal(t, "linux", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[1].MatchExpressions[0].Values[0])
		assert.Equal(t, "amd64", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[1].MatchExpressions[1].Values[0])
	})

	t.Run("no target architecture set, expect only linux affinity", func(t *testing.T) {
		origVM := defaultVm()
		affinity := affinityForVirtualMachine(origVM)
		prettyPrint(t, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
		assert.Equal(t, 1, len(affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms))
		assert.Equal(t, "kubernetes.io/os", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key)
		assert.Equal(t, corev1.NodeSelectorOpIn, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator)
		assert.Equal(t, "linux", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0])
	})

	t.Run("target architecture set, expect linux and architecture affinity", func(t *testing.T) {
		origVM := defaultVm()
		origVM.Spec.TargetArchitecture = lo.ToPtr(vmv1.CPUArchitectureAMD64)
		affinity := affinityForVirtualMachine(origVM)
		prettyPrint(t, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
		assert.Equal(t, "kubernetes.io/os", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key)
		assert.Equal(t, corev1.NodeSelectorOpIn, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator)
		assert.Equal(t, "linux", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0])

		assert.Equal(t, "kubernetes.io/arch", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[1].Key)
		assert.Equal(t, corev1.NodeSelectorOpIn, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[1].Operator)
		assert.Equal(t, "amd64", affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[1].Values[0])
	})
}
