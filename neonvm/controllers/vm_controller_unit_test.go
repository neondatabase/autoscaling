package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
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
	"k8s.io/client-go/tools/record"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/neonvm/controllers/mocks"
	"github.com/neondatabase/autoscaling/neonvm/qmp"
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
					Min: lo.ToPtr(vmv1.MilliCPU(1000)),
					Max: lo.ToPtr(vmv1.MilliCPU(2000)),
					Use: lo.ToPtr(vmv1.MilliCPU(1500)),
				},
				MemorySlots: vmv1.MemorySlots{
					Min: lo.ToPtr(int32(512)),
					Max: lo.ToPtr(int32(2048)),
					Use: lo.ToPtr(int32(1024)),
				},
				MemorySlotSize: *resource.NewQuantity(1, resource.DecimalSI),
			},
		},
		Status: vmv1.VirtualMachineStatus{
			Phase:         "",
			Conditions:    []metav1.Condition{},
			RestartCount:  0,
			PodName:       "",
			PodIP:         "",
			ExtraNetIP:    "",
			ExtraNetMask:  "",
			Node:          "",
			CPUs:          lo.ToPtr(vmv1.MilliCPU(100)),
			MemorySize:    resource.NewQuantity(123, resource.DecimalSI),
			SSHSecretName: "",
		},
	}
}

type testParams struct {
	t              *testing.T
	ctx            context.Context
	r              *VMReconciler
	client         client.Client
	origVM         *vmv1.VirtualMachine
	mockRecorder   *mockRecorder
	mockQMPFactory *mockQMPFactory
	mockHTTPClient *mocks.HTTPClient
}

var reconcilerMetrics = MakeReconcilerMetrics()

type mockQMPFactory struct {
	mock.Mock
}

func (m *mockQMPFactory) ConnectVM(vm *vmv1.VirtualMachine) (QMPMonitor, error) {
	call := m.Called(vm)
	return call.Get(0).(QMPMonitor), call.Error(1)
}

func (m *mockQMPFactory) QmpSetMemorySlots(
	ctx context.Context,
	vm *vmv1.VirtualMachine,
	slots int,
	recorder record.EventRecorder,
) (int, error) {
	call := m.Called(ctx, vm, slots, recorder)
	return call.Int(0), call.Error(1)
}

func newTestParams(t *testing.T) *testParams {
	os.Setenv("VM_RUNNER_IMAGE", "vm-runner-img")

	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stdout),
		zap.Level(zapcore.DebugLevel))
	ctx := log.IntoContext(context.Background(), logger)

	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(vmv1.SchemeGroupVersion, &vmv1.VirtualMachine{})
	scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.Pod{})

	params := &testParams{
		t:      t,
		ctx:    ctx,
		client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		//nolint:exhaustruct // This is a mock
		mockRecorder: &mockRecorder{},
		r:            nil,
		origVM:       nil,
		//nolint:exhaustruct // This is a mock
		mockQMPFactory: &mockQMPFactory{},
		mockHTTPClient: mocks.NewHTTPClient(t),
	}

	params.r = &VMReconciler{
		Client:   params.client,
		Recorder: params.mockRecorder,
		Scheme:   scheme,
		Config: &ReconcilerConfig{
			IsK3s:                   false,
			UseContainerMgr:         false,
			MaxConcurrentReconciles: 10,
			QEMUDiskCacheSettings:   "",
			FailurePendingPeriod:    time.Minute,
			FailingRefreshInterval:  time.Minute,
		},
		Metrics:    reconcilerMetrics,
		QMPFactory: params.mockQMPFactory,
		HTTPClient: params.mockHTTPClient,
	}

	return params
}

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

//nolint:unused // This is used to catch recorder calls during test debug
func (p *testParams) recorderCatchAll() {
	p.mockRecorder.
		On("Eventf", mock.Anything, mock.Anything,
			mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()
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
	// Spec is unchanged
	assert.Equal(t, vm.Spec, origVM.Spec)

	// Round 4
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// Pod is pending, so nothing changes
	assert.Equal(t, vm, params.getVM())
}

func PrettyPrint(t *testing.T, obj any) {
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
		mock.Anything).Once()
	res, err := params.r.Reconcile(params.ctx, req)
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

	PrettyPrint(t, pod)

	pod.Status.Phase = corev1.PodRunning
	err = params.client.Update(params.ctx, &pod)
	require.NoError(t, err)

	// Round 2
	res, err = params.r.Reconcile(params.ctx, req)
	require.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	vm := params.getVM()

	// VM is now running
	assert.Equal(t, vmv1.VmRunning, vm.Status.Phase)
	assert.Len(t, vm.Status.Conditions, 1)
	assert.Equal(t, vm.Status.Conditions[0].Type, typeAvailableVirtualMachine)

	// Round 3
	// Now QMP kicks in
	// params.recorderCatchAll()

	mon := mocks.NewQMPMonitor(t)
	params.mockQMPFactory.On("ConnectVM", vm).Return(mon, nil)

	params.mockRecorder.On("Event", mock.Anything, "Normal", "CpuInfo", mock.Anything).
		Return(nil)
	params.mockRecorder.On("Event", mock.Anything, "Normal", "MemoryInfo", mock.Anything).
		Return(nil)

	result := `{}`
	//nolint:exhaustruct // This is a test
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(result)),
	}
	params.mockHTTPClient.On("Do", mock.Anything).Return(resp, nil)

	memorySize := resource.NewQuantity(int64(1000), resource.DecimalSI)
	mon.On("CPUs").Return([]qmp.CPUSlot{}, []qmp.CPUSlot{}, nil).Once()
	mon.On("MemorySize").Return(memorySize, nil).Once()

	mon.On("Close")

	res, err = params.r.Reconcile(params.ctx, req)
	require.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// Status is updated
	vm.Status.Phase = vmv1.VmScaling
	vm.Status.CPUs = lo.ToPtr(vmv1.MilliCPU(0))
	vm.Status.MemorySize = resource.NewScaledQuantity(1, 3)

	// Need to call this to update the internal representation of the
	// resource.Quantity
	_ = vm.Status.MemorySize.String()

	assert.Equal(t, vm.Status, params.getVM().Status)
}
