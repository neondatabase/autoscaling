package controllers

// Wrapper around the default VirtualMachine/VirtualMachineMigration webhook interfaces so that the
// controller has a bit more control over them, without needing to actually implement that control
// inside of the apis package.

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type VMWebhook struct {
	Recorder record.EventRecorder
	Config   *ReconcilerConfig
}

func (w *VMWebhook) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&vmv1.VirtualMachine{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

var _ webhook.CustomDefaulter = (*VMWebhook)(nil)

// Default implements webhook.CustomDefaulter
func (w *VMWebhook) Default(ctx context.Context, obj runtime.Object) error {
	vm := obj.(*vmv1.VirtualMachine)
	vm.Default()
	return nil
}

var _ webhook.CustomValidator = (*VMWebhook)(nil)

// ValidateCreate implements webhook.CustomValidator
func (w *VMWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	vm := obj.(*vmv1.VirtualMachine)
	return vm.ValidateCreate()
}

// ValidateUpdate implements webhook.CustomValidator
func (w *VMWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newVM := newObj.(*vmv1.VirtualMachine)
	return newVM.ValidateUpdate(oldObj)
}

// ValidateDelete implements webhook.CustomValidator
func (w *VMWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	vm := obj.(*vmv1.VirtualMachine)
	return vm.ValidateDelete()
}

type VMMigrationWebhook struct {
	Recorder record.EventRecorder
	Config   *ReconcilerConfig
}

func (w *VMMigrationWebhook) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&vmv1.VirtualMachineMigration{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

var _ webhook.CustomDefaulter = (*VMWebhook)(nil)

// Default implements webhook.CustomDefaulter
func (w *VMMigrationWebhook) Default(ctx context.Context, obj runtime.Object) error {
	vmm := obj.(*vmv1.VirtualMachineMigration)
	vmm.Default()
	return nil
}

var _ webhook.CustomValidator = (*VMWebhook)(nil)

// ValidateCreate implements webhook.CustomValidator
func (w *VMMigrationWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	vmm := obj.(*vmv1.VirtualMachineMigration)
	return vmm.ValidateCreate()
}

// ValidateUpdate implements webhook.CustomValidator
func (w *VMMigrationWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newVMM := newObj.(*vmv1.VirtualMachineMigration)
	return newVMM.ValidateUpdate(oldObj)
}

// ValidateDelete implements webhook.CustomValidator
func (w *VMMigrationWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	vmm := obj.(*vmv1.VirtualMachineMigration)
	return vmm.ValidateDelete()
}
