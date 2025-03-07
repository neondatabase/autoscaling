/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"fmt"
	"reflect"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"k8s.io/apimachinery/pkg/runtime"
)

//+kubebuilder:webhook:path=/mutate-vm-neon-tech-v1-virtualmachinemigration,mutating=true,failurePolicy=fail,sideEffects=None,groups=vm.neon.tech,resources=virtualmachinemigrations,verbs=create;update,versions=v1,name=mvirtualmachinemigration.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &VirtualMachineMigration{}

// Default implements webhook.Defaulter
//
// The controller wraps this logic so it can inject extra control in the webhook.
func (r *VirtualMachineMigration) Default() {
	// TODO: implement defaults
}

//+kubebuilder:webhook:path=/validate-vm-neon-tech-v1-virtualmachinemigration,mutating=false,failurePolicy=fail,sideEffects=None,groups=vm.neon.tech,resources=virtualmachinemigrations,verbs=create;update,versions=v1,name=vvirtualmachinemigration.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &VirtualMachineMigration{}

// ValidateCreate implements webhook.Validator
//
// The controller wraps this logic so it can inject extra control in the webhook.
func (r *VirtualMachineMigration) ValidateCreate() (admission.Warnings, error) {
	if len(r.Spec.VmName) == 0 {
		return nil, fmt.Errorf("vmName is required")
	}
	return nil, nil
}

// ValidateUpdate implements webhook.Validator
//
// The controller wraps this logic so it can inject extra control in the webhook.
func (r *VirtualMachineMigration) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	oldVMM := old.(*VirtualMachineMigration)
	if !reflect.DeepEqual(r.Spec, oldVMM.Spec) {
		return nil, fmt.Errorf("updating migration spec is not allowed")
	}
	return nil, nil
}

// ValidateDelete implements webhook.Validator
//
// The controller wraps this logic so it can inject extra control in the webhook.
func (r *VirtualMachineMigration) ValidateDelete() (admission.Warnings, error) {
	// No validation needed - we have a finalizer that prevents deletion
	return nil, nil
}
