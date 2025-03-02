package controllers

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type migrationTestParams struct {
	t            *testing.T
	ctx          context.Context
	r            *VirtualMachineMigrationReconciler
	client       client.Client
	mockRecorder *mockRecorder
}

func newMigrationTestParams(t *testing.T) *migrationTestParams {
	os.Setenv("VM_RUNNER_IMAGE", "vm-runner-img")

	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stdout),
		zap.Level(zapcore.DebugLevel))
	ctx := log.IntoContext(context.Background(), logger)

	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(vmv1.SchemeGroupVersion, &vmv1.VirtualMachine{})
	scheme.AddKnownTypes(vmv1.SchemeGroupVersion, &vmv1.VirtualMachineMigration{})
	scheme.AddKnownTypes(corev1.SchemeGroupVersion, &corev1.Pod{})

	params := &migrationTestParams{
		t:   t,
		ctx: ctx,
		client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&vmv1.VirtualMachine{}).
			WithStatusSubresource(&vmv1.VirtualMachineMigration{}).
			Build(),
		//nolint:exhaustruct // This is a mock
		mockRecorder: &mockRecorder{},
		r:            nil,
	}

	params.r = &VirtualMachineMigrationReconciler{
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
		Metrics: testReconcilerMetrics,
	}

	return params
}

// initVM initializes the VM in the fake client and returns the VM
func (p *migrationTestParams) createVM(vm *vmv1.VirtualMachine) {
	err := p.client.Create(p.ctx, vm)
	require.NoError(p.t, err)

	p.refetchVM(vm)
}

func (p *migrationTestParams) refetchVM(vm *vmv1.VirtualMachine) {
	err := p.client.Get(p.ctx, client.ObjectKeyFromObject(vm), vm)
	require.NoError(p.t, err)
}

func (p *migrationTestParams) createMigration(vmm *vmv1.VirtualMachineMigration) {
	err := p.client.Create(p.ctx, vmm)
	require.NoError(p.t, err)

	p.refetchMigration(vmm)
}

func (p *migrationTestParams) refetchMigration(vmm *vmv1.VirtualMachineMigration) {
	err := p.client.Get(p.ctx, client.ObjectKeyFromObject(vmm), vmm)
	require.NoError(p.t, err)
}

func (p *migrationTestParams) reconcileSuccess(vmm *vmv1.VirtualMachineMigration) {
	req := reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(vmm),
	}
	res, err := p.r.Reconcile(p.ctx, req)
	require.NoError(p.t, err)
	require.Equal(p.t, reconcile.Result{}, res)

	p.refetchMigration(vmm)
}

func (p *migrationTestParams) reconcileTimeout(vmm *vmv1.VirtualMachineMigration) {
	req := reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(vmm),
	}
	res, err := p.r.Reconcile(p.ctx, req)
	require.NoError(p.t, err)
	require.Equal(p.t, reconcile.Result{RequeueAfter: time.Second}, res)

	p.refetchMigration(vmm)
}

func (p *migrationTestParams) migrationPrePending(vmm *vmv1.VirtualMachineMigration) {
	// Phase 0: set finalizer
	p.reconcileSuccess(vmm)

	p.refetchMigration(vmm)

	require.Equal(p.t, vmm.Finalizers, []string{virtualmachinemigrationFinalizer})
	require.Equal(p.t, vmm.Status.Phase, vmv1.VmmPhase(""))
	require.Equal(p.t, vmm.Status.TargetPodName, "")

	// Phase 1: set owner
	p.reconcileSuccess(vmm)

	// Phase 2: set conditions
	p.reconcileSuccess(vmm)

	// Phase 3: set target pod name
	p.reconcileSuccess(vmm)

	require.Contains(p.t, vmm.Status.TargetPodName, "test-vm")
}

func (p *migrationTestParams) migrationToPending(vmm *vmv1.VirtualMachineMigration) {
	p.migrationPrePending(vmm)
	// Phase 4: set pending
	p.reconcileSuccess(vmm)

	require.Equal(p.t, vmm.Status.Phase, vmv1.VmmPending)
}

func Test_VMM_to_Pending(t *testing.T) {
	params := newMigrationTestParams(t)
	vm := defaultVm()
	vm.Status.Phase = vmv1.VmRunning
	vm.Status.PodIP = "1.2.3.4"
	params.createVM(vm)

	vmm := &vmv1.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-migration",
			Namespace: vm.Namespace,
		},
		Spec: vmv1.VirtualMachineMigrationSpec{
			VmName: vm.Name,
		},
	}
	params.createMigration(vmm)

	require.Empty(t, vmm.Finalizers)

	params.migrationToPending(vmm)

	params.refetchVM(vm)
	require.Equal(t, vmm.Status.Phase, vmv1.VmmPending)
	require.Equal(t, vm.Status.Phase, vmv1.VmPreMigrating)
}

func Test_VMM_twice_to_Pending(t *testing.T) {
	params := newMigrationTestParams(t)
	vm := defaultVm()
	vm.Status.Phase = vmv1.VmRunning
	vm.Status.PodIP = "1.2.3.4"
	params.createVM(vm)

	vmm1 := &vmv1.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-migration-1",
			Namespace: vm.Namespace,
		},
		Spec: vmv1.VirtualMachineMigrationSpec{
			VmName: vm.Name,
		},
	}
	params.createMigration(vmm1)

	vmm2 := &vmv1.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-migration-2",
			Namespace: vm.Namespace,
		},
		Spec: vmv1.VirtualMachineMigrationSpec{
			VmName: vm.Name,
		},
	}
	params.createMigration(vmm2)

	params.migrationToPending(vmm1)

	// Second migration proceeds until it detects VM is not running

	params.migrationPrePending(vmm2)
	params.reconcileTimeout(vmm2)

	// TODO: should it fail instead?
}

func Test_VMM_to_Pending_then_removed(t *testing.T) {
	params := newMigrationTestParams(t)
	vm := defaultVm()
	vm.Status.Phase = vmv1.VmRunning
	vm.Status.PodIP = "1.2.3.4"
	params.createVM(vm)

	vmm := &vmv1.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-migration",
			Namespace: vm.Namespace,
		},
		Spec: vmv1.VirtualMachineMigrationSpec{
			VmName: vm.Name,
		},
	}
	params.createMigration(vmm)

	params.migrationToPending(vmm)
	params.refetchVM(vm)
	require.Equal(t, vmm.Status.Phase, vmv1.VmmPending)
	require.Equal(t, vm.Status.Phase, vmv1.VmPreMigrating)

	err := params.client.Delete(params.ctx, vmm)
	require.NoError(t, err)

	params.mockRecorder.On("Event", mock.Anything, "Warning", "Deleting", "Running Migration (test-migration) is being deleted")

	req := reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(vmm),
	}
	res, err := params.r.Reconcile(params.ctx, req)
	require.NoError(params.t, err)
	require.Equal(params.t, reconcile.Result{}, res)

	// Migration it deleted by that point
	err = params.client.Get(params.ctx, client.ObjectKeyFromObject(vmm), vmm)
	require.Error(params.t, err)

	params.refetchVM(vm)
	require.Equal(params.t, vm.Status.Phase, vmv1.VmRunning)
}

// erroringClient wraps a client.Client and returns an error for specific operations
type erroringClient struct {
	client.Client
	shouldErrorOnUpdate func(obj client.Object) bool
}

// Update returns an error when shouldErrorOnUpdate returns true
func (c *erroringClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.shouldErrorOnUpdate(obj) {
		return errors.New("simulated error updating object")
	}
	return c.Client.Update(ctx, obj, opts...)
}

// Status returns a subresource client that errors on status updates
func (c *erroringClient) Status() client.StatusWriter {
	return &erroringStatusWriter{
		StatusWriter:        c.Client.Status(),
		shouldErrorOnUpdate: c.shouldErrorOnUpdate,
	}
}

// erroringStatusWriter wraps a StatusWriter and returns an error for specific operations
type erroringStatusWriter struct {
	client.StatusWriter
	shouldErrorOnUpdate func(obj client.Object) bool
}

// Update returns an error when shouldErrorOnUpdate returns true
func (sw *erroringStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if sw.shouldErrorOnUpdate(obj) {
		return errors.New("simulated error updating status")
	}
	return sw.StatusWriter.Update(ctx, obj, opts...)
}

func Test_VMM_failure_updating_migration_status(t *testing.T) {
	params := newMigrationTestParams(t)

	// Create VM in Running state and attach migration
	vm := defaultVm()
	vm.Status.Phase = vmv1.VmRunning
	vm.Status.PodIP = "1.2.3.4"
	params.createVM(vm)

	vmm := &vmv1.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-migration",
			Namespace: vm.Namespace,
		},
		Spec: vmv1.VirtualMachineMigrationSpec{
			VmName: vm.Name,
		},
	}
	params.createMigration(vmm)
	params.migrationPrePending(vmm)

	// Replace with client that errors on migration status updates
	originalClient := params.client
	params.client = &erroringClient{
		Client: originalClient,
		shouldErrorOnUpdate: func(obj client.Object) bool {
			_, isMigration := obj.(*vmv1.VirtualMachineMigration)
			return isMigration
		},
	}
	params.r.Client = params.client

	// Reconcile and verify error
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vmm)}
	_, err := params.r.Reconcile(params.ctx, req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "simulated error")

	// Verify state after error
	params.client, params.r.Client = originalClient, originalClient
	params.refetchVM(vm)
	params.refetchMigration(vmm)
	require.Equal(t, vm.Status.Phase, vmv1.VmPreMigrating)
	require.Equal(t, vmm.Status.Phase, vmv1.VmmPhase(""))
	// TODO: this is very bad state, we will not be able to escape it.
}
