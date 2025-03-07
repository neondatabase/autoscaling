package controllers

// Wrapper around the default VirtualMachine/VirtualMachineMigration webhook interfaces so that the
// controller has a bit more control over them, without needing to actually implement that control
// inside of the apis package.

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/util/stack"
)

func validateUpdate(
	ctx context.Context,
	cfg *ReconcilerConfig,
	recorder record.EventRecorder,
	oldObj runtime.Object,
	newObj interface {
		webhook.Validator
		metav1.Object
	},
) (admission.Warnings, error) {
	log := log.FromContext(ctx)

	namespacedName := client.ObjectKeyFromObject(newObj)
	_, skipValidation := cfg.SkipUpdateValidationFor[namespacedName]

	warnings, err := func() (w admission.Warnings, e error) {
		// if we plan to skip validation, catch any panics so that they can be ignored.
		if skipValidation {
			defer func() {
				if err := recover(); err != nil {
					e = fmt.Errorf("validation panicked with: %v", err)
					st := stack.GetStackTrace(nil, 1).String()
					log.Error(e, "webhook update validation panicked", "stack", st)
				}
			}()
		}

		return newObj.ValidateUpdate(oldObj)
	}()

	if err != nil && skipValidation {
		recorder.Event(
			newObj,
			"Warning",
			"SkippedValidation",
			"Ignoring failed webhook validation because of controller's '--skip-update-validation-for' flag",
		)
		log.Error(err, "Ignoring failed webhook validation")
		return warnings, nil
	}

	return warnings, err
}

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
	return validateUpdate(ctx, w.Config, w.Recorder, oldObj, newVM)
}

// ValidateDelete implements webhook.CustomValidator
func (w *VMWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	vm := obj.(*vmv1.VirtualMachine)
	return vm.ValidateDelete()
}

type VMMigrationWebhook struct {
	Recorder record.EventRecorder
	Config   *ReconcilerConfig
	Client   client.Client
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
	return validateUpdate(ctx, w.Config, w.Recorder, oldObj, newVMM)
}

// ValidateDelete implements webhook.CustomValidator
func (w *VMMigrationWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	vmm := obj.(*vmv1.VirtualMachineMigration)
	return vmm.ValidateDelete()
}
