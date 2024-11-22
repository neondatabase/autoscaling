package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VirtualMachineOwnerForPod returns the OwnerReference for the VirtualMachine that owns the pod, if
// there is one.
//
// When a live migration is ongoing, only the source Pod will be marked as owned by the
// VirtualMachine.
func VirtualMachineOwnerForPod(pod *corev1.Pod) (_ metav1.OwnerReference, ok bool) {
	gv := SchemeGroupVersion.String()

	for _, ref := range pod.OwnerReferences {
		if ref.APIVersion == gv && ref.Kind == "VirtualMachine" {
			return ref, true
		}
	}

	var empty metav1.OwnerReference
	return empty, false
}

// MigrationRole represents the role that a Pod is taking during a live migration -- either the
// source or target of the migration.
type MigrationRole string

const (
	MigrationRoleSource MigrationRole = "source"
	MigrationRoleTarget MigrationRole = "target"
)

// MigrationOwnerForPod returns the OwnerReference for the live migration that this Pod is a part
// of, if there is one ongoing.
//
// The MigrationRole returned also indicates whether the Pod is the source or the target of the
// migration.
func MigrationOwnerForPod(pod *corev1.Pod) (metav1.OwnerReference, MigrationRole, bool) {
	gv := SchemeGroupVersion.String()

	for _, ref := range pod.OwnerReferences {
		if ref.APIVersion == gv && ref.Kind == "VirtualMachineMigration" {
			var role MigrationRole
			if ref.Controller != nil && *ref.Controller {
				// the migration only ever "controls" the target pod. When the migration is ongoing,
				// the virtual machine controls the source, and when it's over, the migration stops
				// owning the target and transfers "control" to the virtual machine object, while
				// keeping the source pod as a non-controlling reference.
				role = MigrationRoleTarget
			} else {
				role = MigrationRoleSource
			}

			return ref, role, true
		}
	}

	var emptyRef metav1.OwnerReference
	return emptyRef, "", false
}
