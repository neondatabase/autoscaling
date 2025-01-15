package controllers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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

type mockRecorder struct {
	mock.Mock
}

func (m *mockRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	m.Called(object, eventtype, reason, message)
}

func (m *mockRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	m.Called(object, eventtype, reason, messageFmt, args)
}

func (m *mockRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	m.Called(object, annotations, eventtype, reason, messageFmt, args)
}

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
	t            *testing.T
	ctx          context.Context
	r            *VMReconciler
	client       client.Client
	origVM       *vmv1.VirtualMachine
	mockRecorder *mockRecorder
}

var reconcilerMetrics = MakeReconcilerMetrics()

func newTestParams(t *testing.T) *testParams {
	os.Setenv("VM_RUNNER_IMAGE", "vm-runner-img")

	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stdout),
		zap.Level(zapcore.DebugLevel))
	ctx := log.IntoContext(context.Background(), logger)

	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(vmv1.SchemeGroupVersion, &vmv1.VirtualMachine{})
	scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.Pod{})

	params := &testParams{
		t:   t,
		ctx: ctx,
		client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&vmv1.VirtualMachine{}).
			Build(),
		//nolint:exhaustruct // This is a mock
		mockRecorder: &mockRecorder{},
		r:            nil,
		origVM:       nil,
	}

	params.r = &VMReconciler{
		Client:   params.client,
		Recorder: params.mockRecorder,
		Scheme:   scheme,
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
		},
		Metrics: reconcilerMetrics,
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
	params.mockRecorder.On("Event", mock.Anything, "Normal", "Created",
		mock.Anything)
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// We now have a pod
	vm := params.getVM()
	assert.NotEmpty(t, vm.Status.PodName)
	// Spec is unchanged except cpuScalingMode
	var origVMWithCPUScalingMode vmv1.VirtualMachine
	origVM.DeepCopy().DeepCopyInto(&origVMWithCPUScalingMode)
	origVMWithCPUScalingMode.Spec.CpuScalingMode = lo.ToPtr(vmv1.CpuScalingModeQMP)
	assert.Equal(t, vm.Spec, origVMWithCPUScalingMode.Spec)

	// Round 4
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// Nothing is updating the pod status, so nothing changes in VM as well
	assert.Equal(t, vm, params.getVM())
}

func prettyPrint(t *testing.T, obj any) {
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
	params.mockRecorder.On("Event", mock.Anything, "Normal", "Created",
		mock.Anything)
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
	err = params.client.Status().Update(params.ctx, &pod)
	require.NoError(t, err)
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
