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
	"errors"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/api/resource"
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
	// CPU spec defaulter
	if r.Spec.Guest.CPUs.Use == nil {
		r.Spec.Guest.CPUs.Use = new(MilliCPU)
		*r.Spec.Guest.CPUs.Use = *r.Spec.Guest.CPUs.Min
		virtualmachinelog.Info("defaulting guest CPU settings", ".spec.guest.cpus.use", *r.Spec.Guest.CPUs.Use)
	}
	if r.Spec.Guest.CPUs.Max == nil {
		r.Spec.Guest.CPUs.Max = new(MilliCPU)
		*r.Spec.Guest.CPUs.Max = *r.Spec.Guest.CPUs.Min
		virtualmachinelog.Info("defaulting guest CPU settings", ".spec.guest.cpus.max", *r.Spec.Guest.CPUs.Max)
	}

	// Memory spec defaulter
	if r.Spec.Guest.Memory != nil {
		if r.Spec.Guest.Memory.Use == nil {
			r.Spec.Guest.Memory.Use = new(resource.Quantity)
			*r.Spec.Guest.Memory.Use = *r.Spec.Guest.Memory.Min
			virtualmachinelog.Info("defaulting guest memory settings", ".spec.guest.memory.use", *r.Spec.Guest.Memory.Use)
		}
		if r.Spec.Guest.Memory.Max == nil {
			r.Spec.Guest.Memory.Max = new(resource.Quantity)
			*r.Spec.Guest.Memory.Max = *r.Spec.Guest.Memory.Min
			virtualmachinelog.Info("defaulting guest memory settings", ".spec.guest.memory.max", *r.Spec.Guest.Memory.Max)
		}
	} else {
		// MemorySlots spec defaulter
		if r.Spec.Guest.MemorySlots.Use == nil {
			r.Spec.Guest.MemorySlots.Use = new(int32)
			*r.Spec.Guest.MemorySlots.Use = *r.Spec.Guest.MemorySlots.Min
			virtualmachinelog.Info("defaulting guest memory settings", ".spec.guest.memorySlots.use", *r.Spec.Guest.MemorySlots.Use)
		}
		if r.Spec.Guest.MemorySlots.Max == nil {
			r.Spec.Guest.MemorySlots.Max = new(int32)
			*r.Spec.Guest.MemorySlots.Max = *r.Spec.Guest.MemorySlots.Min
			virtualmachinelog.Info("defaulting guest memory settings", ".spec.guest.memorySlots.max", *r.Spec.Guest.MemorySlots.Max)
		}
	}
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
//+kubebuilder:webhook:path=/validate-vm-neon-tech-v1-virtualmachine,mutating=false,failurePolicy=fail,sideEffects=None,groups=vm.neon.tech,resources=virtualmachines,verbs=create;update,versions=v1,name=vvirtualmachine.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &VirtualMachine{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *VirtualMachine) ValidateCreate() error {
	// validate .spec.guest.cpus.use and .spec.guest.cpus.max
	if r.Spec.Guest.CPUs.Use != nil {
		if r.Spec.Guest.CPUs.Max == nil {
			return errors.New(".spec.guest.cpus.max must be defined if .spec.guest.cpus.use specified")
		}
		if *r.Spec.Guest.CPUs.Use < *r.Spec.Guest.CPUs.Min {
			return fmt.Errorf(".spec.guest.cpus.use (%v) should be greater than or equal to the .spec.guest.cpus.min (%v)",
				r.Spec.Guest.CPUs.Use,
				r.Spec.Guest.CPUs.Min)
		}
		if *r.Spec.Guest.CPUs.Use > *r.Spec.Guest.CPUs.Max {
			return fmt.Errorf(".spec.guest.cpus.use (%v) should be less than or equal to the .spec.guest.cpus.max (%v)",
				r.Spec.Guest.CPUs.Use,
				r.Spec.Guest.CPUs.Max)
		}
	}

	if r.Spec.Guest.Memory == nil && r.Spec.Guest.MemorySlots == nil {
		return errors.New("either .spec.guest.memory or .spec.guest.memorySlots must be specified")
	}
	if r.Spec.Guest.Memory != nil && r.Spec.Guest.MemorySlots != nil {
		return errors.New("only .spec.guest.memory or .spec.guest.memorySlots must be specified")
	}
	if r.Spec.Guest.Memory != nil {
		// validate .spec.guest.memory.use and .spec.guest.memory.max
		if r.Spec.Guest.Memory.Use != nil {
			if r.Spec.Guest.Memory.Max == nil {
				return errors.New(".spec.guest.memory.max must be defined if .spec.guest.memory.use specified")
			}
			if r.Spec.Guest.Memory.Use.Cmp(*r.Spec.Guest.Memory.Min) < 0 {
				return fmt.Errorf(".spec.guest.memory.use (%s) should be greater than or equal to the .spec.guest.memory.min (%s)",
					r.Spec.Guest.Memory.Use.String(),
					r.Spec.Guest.Memory.Min.String())
			}
			if r.Spec.Guest.Memory.Use.Cmp(*r.Spec.Guest.Memory.Max) > 0 {
				return fmt.Errorf(".spec.guest.memory.use (%s) should be less than or equal to the .spec.guest.memory.max (%s)",
					r.Spec.Guest.Memory.Use.String(),
					r.Spec.Guest.Memory.Max.String())
			}
			if err := MemorySizeValidate(r.Spec.Guest.Memory.Min); err != nil {
				return err
			}
			if err := MemorySizeValidate(r.Spec.Guest.Memory.Max); err != nil {
				return err
			}
			if err := MemorySizeValidate(r.Spec.Guest.Memory.Use); err != nil {
				return err
			}
		}
	} else {
		// validate .spec.guest.memorySlots.use and .spec.guest.memorySlots.max
		if r.Spec.Guest.MemorySlots.Use != nil {
			if r.Spec.Guest.MemorySlots.Max == nil {
				return errors.New(".spec.guest.memorySlots.max must be defined if .spec.guest.memorySlots.use specified")
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
	}

	// validate .spec.disk names
	for _, disk := range r.Spec.Disks {
		if disk.Name == "virtualmachineimages" {
			return errors.New("'virtualmachineimages' is reserved name for .spec.disks[].name")
		}
		if disk.Name == "vmroot" {
			return errors.New("'vmroot' is reserved name for .spec.disks[].name")
		}
		if disk.Name == "vmruntime" {
			return errors.New("'vmruntime' is reserved name for .spec.disks[].name")
		}
		if len(disk.Name) > 32 {
			return fmt.Errorf("disk name '%s' too long, should be less than or equal to 32", disk.Name)
		}
	}

	// validate .spec.guest.ports[].name
	for _, port := range r.Spec.Guest.Ports {
		if len(port.Name) != 0 && port.Name == "qmp" {
			return errors.New("'qmp' is reserved name for .spec.guest.ports[].name")
		}
	}

	return nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *VirtualMachine) ValidateUpdate(old runtime.Object) error {
	// process immutable fields
	before, _ := old.(*VirtualMachine)

	immutableFields := []struct {
		fieldName string
		getter    func(*VirtualMachine) any
	}{
		{".spec.guest.cpus.min", func(v *VirtualMachine) any { return v.Spec.Guest.CPUs.Min }},
		{".spec.guest.cpus.max", func(v *VirtualMachine) any { return v.Spec.Guest.CPUs.Max }},
		{".spec.guest.memory.min", func(v *VirtualMachine) any {
			if v.Spec.Guest.Memory != nil {
				return v.Spec.Guest.Memory.Min
			} else {
				return nil
			}
		}},
		{".spec.guest.memory.max", func(v *VirtualMachine) any {
			if v.Spec.Guest.Memory != nil {
				return v.Spec.Guest.Memory.Max
			} else {
				return nil
			}
		}},
		{".spec.guest.memorySlots.min", func(v *VirtualMachine) any {
			if v.Spec.Guest.MemorySlots != nil {
				return v.Spec.Guest.MemorySlots.Min
			} else {
				return nil
			}
		}},
		{".spec.guest.memorySlots.max", func(v *VirtualMachine) any {
			if v.Spec.Guest.MemorySlots != nil {
				return v.Spec.Guest.MemorySlots.Max
			} else {
				return nil
			}
		}},
		{".spec.guest.ports", func(v *VirtualMachine) any { return v.Spec.Guest.Ports }},
		{".spec.guest.rootDisk", func(v *VirtualMachine) any { return v.Spec.Guest.RootDisk }},
		{".spec.guest.command", func(v *VirtualMachine) any { return v.Spec.Guest.Command }},
		{".spec.guest.args", func(v *VirtualMachine) any { return v.Spec.Guest.Args }},
		{".spec.guest.env", func(v *VirtualMachine) any { return v.Spec.Guest.Env }},
		{".spec.guest.settings", func(v *VirtualMachine) any { return v.Spec.Guest.Settings }},
		{".spec.disk", func(v *VirtualMachine) any { return v.Spec.Disks }},
		{".spec.podResources", func(v *VirtualMachine) any { return v.Spec.PodResources }},
		{".spec.enableAcceleration", func(v *VirtualMachine) any { return v.Spec.EnableAcceleration }},
	}

	for _, info := range immutableFields {
		if !reflect.DeepEqual(info.getter(r), info.getter(before)) {
			return fmt.Errorf("%s is immutable", info.fieldName)
		}
	}

	// validate .spec.guest.cpu.use
	if r.Spec.Guest.CPUs.Use != nil {
		if *r.Spec.Guest.CPUs.Use < *r.Spec.Guest.CPUs.Min {
			return fmt.Errorf(".cpus.use (%v) should be greater than or equal to the .cpus.min (%v)",
				r.Spec.Guest.CPUs.Use,
				r.Spec.Guest.CPUs.Min)
		}
		if *r.Spec.Guest.CPUs.Use > *r.Spec.Guest.CPUs.Max {
			return fmt.Errorf(".cpus.use (%v) should be less than or equal to the .cpus.max (%v)",
				r.Spec.Guest.CPUs.Use,
				r.Spec.Guest.CPUs.Max)
		}
	}

	if r.Spec.Guest.Memory != nil {
		// validate .spec.guest.memory.use
		if r.Spec.Guest.Memory.Use != nil {
			if r.Spec.Guest.Memory.Use.Cmp(*r.Spec.Guest.Memory.Min) < 0 {
				return fmt.Errorf(".spec.guest.memory.use (%s) should be greater than or equal to the .spec.guest.memory.min (%s)",
					r.Spec.Guest.Memory.Use.String(),
					r.Spec.Guest.Memory.Min.String())
			}
			if r.Spec.Guest.Memory.Use.Cmp(*r.Spec.Guest.Memory.Max) > 0 {
				return fmt.Errorf(".spec.guest.memory.use (%s) should be less than or equal to the .spec.guest.memory.max (%s)",
					r.Spec.Guest.Memory.Use.String(),
					r.Spec.Guest.Memory.Max.String())
			}
			if err := MemorySizeValidate(r.Spec.Guest.Memory.Use); err != nil {
				return err
			}
		}
	}

	if r.Spec.Guest.MemorySlots != nil {
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
	}

	return nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *VirtualMachine) ValidateDelete() error {
	// TODO(user): fill in your validation logic upon object deletion.
	return nil
}

func MemorySizeValidate(m *resource.Quantity) error {
	blockSize, err := resource.ParseQuantity("8Mi")
	if err != nil {
		return err
	}
	if m.Value() % blockSize.Value() != 0 {
		return fmt.Errorf("size %s has to be multiples of 'block-size' %s", m.String(), blockSize.String())
	}
	return nil
}
