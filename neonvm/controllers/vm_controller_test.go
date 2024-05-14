package controllers

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type clientMock struct {
	objects map[types.NamespacedName]client.Object
	t       *testing.T
}

func newClientMock(t *testing.T) *clientMock {
	return &clientMock{
		objects: make(map[types.NamespacedName]client.Object),
		t:       t,
	}
}

func (c *clientMock) Get(ctx context.Context, key client.ObjectKey,
	obj client.Object, opts ...client.GetOption) error {

	switch obj.(type) {
	case *vmv1.VirtualMachine:
		ptr := obj.(*vmv1.VirtualMachine)
		vm := c.objects[key].(*vmv1.VirtualMachine)
		*ptr = *vm
		return nil
	}

	return apierrors.NewNotFound(schema.GroupResource{}, key.Name)
}

func (c *clientMock) Update(ctx context.Context, obj client.Object,
	opts ...client.UpdateOption) error {

	c.objects[client.ObjectKeyFromObject(obj)] = obj

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

	c.objects[client.ObjectKeyFromObject(obj)] = obj

	return nil
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

func TestReconcile(t *testing.T) {
	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stdout),
		zap.Level(zapcore.DebugLevel))
	ctx := log.IntoContext(context.Background(), logger)

	clientMock := newClientMock(t)

	testVM := &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm",
			Namespace: "default",
		},
		Spec: vmv1.VirtualMachineSpec{
			EnableSSH:          util.Ptr(false),
			EnableAcceleration: util.Ptr(true),
			Guest: vmv1.Guest{
				CPUs: vmv1.CPUs{
					Use: util.Ptr(vmv1.MilliCPU(1000)),
				},
				MemorySlots: vmv1.MemorySlots{
					Use: util.Ptr(int32(1024)),
				},
			},
		},
		Status: vmv1.VirtualMachineStatus{},
	}

	key := client.ObjectKeyFromObject(testVM)
	clientMock.objects[key] = testVM

	getVM := func() *vmv1.VirtualMachine {
		obj := clientMock.objects[key]
		return obj.(*vmv1.VirtualMachine)
	}

	mockRecorder := &mockRecorder{}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(vmv1.SchemeGroupVersion, testVM)

	reconcilerMetrics := MakeReconcilerMetrics()
	r := &VirtualMachineReconciler{
		Client:   clientMock,
		Recorder: mockRecorder,
		Scheme:   scheme,
		Config:   &ReconcilerConfig{},
		Metrics:  reconcilerMetrics,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-vm",
			Namespace: "default",
		},
	}

	// Round 1
	res, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)

	// Added finalizer
	assert.Equal(t, reconcile.Result{
		Requeue: true,
	}, res)
	assert.Contains(t, getVM().Finalizers, virtualmachineFinalizer)

	// Round 2
	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// VM is pending
	assert.Equal(t, vmv1.VmPending, getVM().Status.Phase)
	assert.Equal(t, 1, len(clientMock.objects))

	// Round 3
	os.Setenv("VM_RUNNER_IMAGE", "vm-runner-img")
	mockRecorder.On("Event", mock.Anything, "Normal", "Created",
		mock.Anything)
	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, false, res.Requeue)

	// We now have a pod
	assert.Equal(t, 2, len(clientMock.objects))

}
