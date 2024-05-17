package controllers

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type clientMock struct {
	objects map[types.NamespacedName][]byte
	t       *testing.T
}

func newClientMock(t *testing.T) *clientMock {
	return &clientMock{
		objects: make(map[types.NamespacedName][]byte),
		t:       t,
	}
}

func (c *clientMock) Get(ctx context.Context, key client.ObjectKey,
	obj client.Object, opts ...client.GetOption) error {
	str, ok := c.objects[key]
	if !ok {
		return apierrors.NewNotFound(vmv1.Resource("virtualmachine"), key.Name)
	}
	err := json.Unmarshal(str, obj)
	require.NoError(c.t, err)

	return nil
}

func (c *clientMock) Update(ctx context.Context, obj client.Object,
	opts ...client.UpdateOption) error {

	str, err := json.Marshal(obj)
	require.NoError(c.t, err)

	c.objects[client.ObjectKeyFromObject(obj)] = str
	return nil
}

func (c *clientMock) Status() client.StatusWriter {
	return c
}

func (c *clientMock) List(ctx context.Context, list client.ObjectList,
	opts ...client.ListOption) error {
	c.t.Fatal("not implemented")
	return nil
}

func (c *clientMock) Create(ctx context.Context, obj client.Object,
	opts ...client.CreateOption) error {
	return c.Update(ctx, obj)
}

func (c *clientMock) Delete(ctx context.Context, obj client.Object,
	opts ...client.DeleteOption) error {
	c.t.Fatal("not implemented")
	return nil
}

func (c *clientMock) Patch(ctx context.Context, obj client.Object,
	patch client.Patch, opts ...client.PatchOption) error {

	c.t.Fatal("not implemented")
	return nil
}

func (c *clientMock) DeleteAllOf(ctx context.Context, obj client.Object,
	opts ...client.DeleteAllOfOption) error {
	c.t.Fatal("not implemented")
	return nil
}

func (c *clientMock) RESTMapper() meta.RESTMapper {
	c.t.Fatal("not implemented")
	return nil
}

func (c *clientMock) Scheme() *runtime.Scheme {
	c.t.Fatal("not implemented")
	return nil
}

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

func defaultVm() *vmv1.VirtualMachine {
	return &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm",
			Namespace: "default",
		},
		Spec: vmv1.VirtualMachineSpec{
			EnableSSH:          util.Ptr(false),
			EnableAcceleration: util.Ptr(true),
			//nolint:exhaustruct // This is a test
			Guest: vmv1.Guest{
				KernelImage:         util.Ptr("kernel-img"),
				AppendKernelCmdline: nil,
				CPUs: vmv1.CPUs{
					Min: util.Ptr(vmv1.MilliCPU(1000)),
					Max: util.Ptr(vmv1.MilliCPU(2000)),
					Use: util.Ptr(vmv1.MilliCPU(1500)),
				},
				MemorySlots: vmv1.MemorySlots{
					Min: util.Ptr(int32(512)),
					Max: util.Ptr(int32(2048)),
					Use: util.Ptr(int32(1024)),
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
			CPUs:          util.Ptr(vmv1.MilliCPU(100)),
			MemorySize:    resource.NewQuantity(123, resource.DecimalSI),
			SSHSecretName: "",
		},
	}
}

type testParams struct {
	t            *testing.T
	ctx          context.Context
	r            *VirtualMachineReconciler
	client       *clientMock
	origVM       *vmv1.VirtualMachine
	mockRecorder *mockRecorder
}

var reconcilerMetrics = MakeReconcilerMetrics()

func newTestParams(t *testing.T) *testParams {
	os.Setenv("VM_RUNNER_IMAGE", "vm-runner-img")

	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stdout),
		zap.Level(zapcore.DebugLevel))
	ctx := log.IntoContext(context.Background(), logger)

	params := &testParams{
		t:      t,
		ctx:    ctx,
		client: newClientMock(t),
		//nolint:exhaustruct // This is a mock
		mockRecorder: &mockRecorder{},
		r:            nil,
		origVM:       nil,
	}

	scheme := runtime.NewScheme()
	var vm *vmv1.VirtualMachine
	scheme.AddKnownTypes(vmv1.SchemeGroupVersion, vm)

	params.r = &VirtualMachineReconciler{
		Client:   params.client,
		Recorder: params.mockRecorder,
		Scheme:   scheme,
		Config: &ReconcilerConfig{
			IsK3s:                   false,
			UseContainerMgr:         false,
			MaxConcurrentReconciles: 10,
			QEMUDiskCacheSettings:   "",
		},
		Metrics: reconcilerMetrics,
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
	assert.Equal(t, 1, len(params.client.objects))

	// Round 3
	params.mockRecorder.On("Event", mock.Anything, "Normal", "Created",
		mock.Anything)
	res, err = params.r.Reconcile(params.ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// We now have a pod
	assert.Equal(t, 2, len(params.client.objects))
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
		mock.Anything)
	res, err := params.r.Reconcile(params.ctx, req)
	require.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// We now have a pod
	assert.Equal(t, 2, len(params.client.objects))

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
}
