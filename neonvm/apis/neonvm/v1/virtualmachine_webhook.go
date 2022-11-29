/*
Copyright 2022.

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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// log is for logging in this package.
var virtualmachinelog = logf.Log.WithName("virtualmachine-resource")

func (r *VirtualMachine) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-vm-neon-tech-v1-virtualmachine,mutating=true,failurePolicy=fail,sideEffects=None,groups=vm.neon.tech,resources=virtualmachines,verbs=create;update,versions=v1,name=mvirtualmachine.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &VirtualMachine{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *VirtualMachine) Default() {
	virtualmachinelog.Info("default", "name", r.Name)

	// TODO(user): fill in your defaulting logic.
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
//+kubebuilder:webhook:path=/validate-vm-neon-tech-v1-virtualmachine,mutating=false,failurePolicy=fail,sideEffects=None,groups=vm.neon.tech,resources=virtualmachines,verbs=create;update,versions=v1,name=vvirtualmachine.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &VirtualMachine{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *VirtualMachine) ValidateCreate() error {
	virtualmachinelog.Info("validate create", "name", r.Name)

	// validate .spec.guest.cpus.use and .spec.guest.cpus.max
	if r.Spec.Guest.CPUs.Use != nil {
		if r.Spec.Guest.CPUs.Max == nil {
			return fmt.Errorf(".spec.guest.cpus.max must be defined if .spec.guest.cpus.use specified")
		}
		if *r.Spec.Guest.CPUs.Use < *r.Spec.Guest.CPUs.Min {
			return fmt.Errorf(".spec.guest.cpus.use (%d) should be greater than or equal to the .spec.guest.cpus.min (%d)",
				*r.Spec.Guest.CPUs.Use,
				*r.Spec.Guest.CPUs.Min)
		}
		if *r.Spec.Guest.CPUs.Use > *r.Spec.Guest.CPUs.Max {
			return fmt.Errorf(".spec.guest.cpus.use (%d) should be less than or equal to the .spec.guest.cpus.max (%d)",
				*r.Spec.Guest.CPUs.Use,
				*r.Spec.Guest.CPUs.Max)
		}
	}

	// validate .spec.guest.memorySlots.use and .spec.guest.memorySlots.max
	if r.Spec.Guest.MemorySlots.Use != nil {
		if r.Spec.Guest.MemorySlots.Max == nil {
			return fmt.Errorf(".spec.guest.memorySlots.max must be defined if .spec.guest.memorySlots.use specified")
		}
		if *r.Spec.Guest.MemorySlots.Use < *r.Spec.Guest.MemorySlots.Min {
			return fmt.Errorf(".spec.guest.memorySlots.use (%d) should be greater than or equal to the .spec.guest.memorySlots.min (%d)",
				*r.Spec.Guest.MemorySlots.Use,
				*r.Spec.Guest.MemorySlots.Min)
		}
		if *r.Spec.Guest.MemorySlots.Use > *r.Spec.Guest.MemorySlots.Max {
			return fmt.Errorf(".spec.guest.memorySlots.use (%d) should be less than or equal to the .spec.guest.memorySlots.max (%d)",
				*r.Spec.Guest.MemorySlots.Use,
				*r.Spec.Guest.MemorySlots.Max)
		}
	}

	// validate .spec.disk names
	for _, disk := range r.Spec.Disks {
		virtualmachinelog.Info("validate disk", "name", disk.Name)
		if disk.Name == "virtualmachineimages" {
			return fmt.Errorf("'virtualmachineimages' is reserved name for .spec.disks[].name")
		}
		if disk.Name == "vmroot" {
			return fmt.Errorf("'vmroot' is reserved name for .spec.disks[].name")
		}
		if disk.Name == "vmruntime" {
			return fmt.Errorf("'vmruntime' is reserved name for .spec.disks[].name")
		}
		if len(disk.Name) > 32 {
			return fmt.Errorf("disk name '%s' too long, should be less than or equal to 32", disk.Name)
		}
	}
	return nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *VirtualMachine) ValidateUpdate(old runtime.Object) error {
	virtualmachinelog.Info("validate update", "name", r.Name)

	// process immutable fields
	before, _ := old.(*VirtualMachine)

	if !reflect.DeepEqual(r.Spec.Guest.CPUs.Min, before.Spec.Guest.CPUs.Min) {
		return fmt.Errorf(".spec.guest.cpus.min is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.CPUs.Max, before.Spec.Guest.CPUs.Max) {
		return fmt.Errorf(".spec.guest.cpus.max is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.MemorySlots.Min, before.Spec.Guest.MemorySlots.Min) {
		return fmt.Errorf(".spec.guest.memorySlots.min is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.MemorySlots.Max, before.Spec.Guest.MemorySlots.Max) {
		return fmt.Errorf(".spec.guest.memorySlots.max is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.Ports, before.Spec.Guest.Ports) {
		return fmt.Errorf(".spec.guest.ports is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.RootDisk, before.Spec.Guest.RootDisk) {
		return fmt.Errorf(".spec.guest.rootDisk is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.Command, before.Spec.Guest.Command) {
		return fmt.Errorf(".spec.guest.command is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.Args, before.Spec.Guest.Args) {
		return fmt.Errorf(".spec.guest.args is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Guest.Env, before.Spec.Guest.Env) {
		return fmt.Errorf(".spec.guest.env is immutable")
	}
	if !reflect.DeepEqual(r.Spec.Disks, before.Spec.Disks) {
		return fmt.Errorf(".spec.disks is immutable")
	}

	// validate .spec.guest.cpu.use
	if r.Spec.Guest.CPUs.Use != nil {
		if *r.Spec.Guest.CPUs.Use < *r.Spec.Guest.CPUs.Min {
			return fmt.Errorf(".cpus.use (%d) should be greater than or equal to the .cpus.min (%d)",
				*r.Spec.Guest.CPUs.Use,
				*r.Spec.Guest.CPUs.Min)
		}
		if *r.Spec.Guest.CPUs.Use > *r.Spec.Guest.CPUs.Max {
			return fmt.Errorf(".cpus.use (%d) should be less than or equal to the .cpus.max (%d)",
				*r.Spec.Guest.CPUs.Use,
				*r.Spec.Guest.CPUs.Max)
		}
	}

	// validate .spec.guest.memorySlots.use
	if r.Spec.Guest.MemorySlots.Use != nil {
		if *r.Spec.Guest.MemorySlots.Use < *r.Spec.Guest.MemorySlots.Min {
			return fmt.Errorf(".memorySlots.use (%d) should be greater than or equal to the .memorySlots.min (%d)",
				*r.Spec.Guest.MemorySlots.Use,
				*r.Spec.Guest.MemorySlots.Min)
		}
		if *r.Spec.Guest.MemorySlots.Use > *r.Spec.Guest.MemorySlots.Max {
			return fmt.Errorf(".memorySlots.use (%d) should be less than or equal to the .memorySlots.max (%d)",
				*r.Spec.Guest.MemorySlots.Use,
				*r.Spec.Guest.MemorySlots.Max)
		}
	}

	return nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *VirtualMachine) ValidateDelete() error {
	virtualmachinelog.Info("validate delete", "name", r.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil
}
