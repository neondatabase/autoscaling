package util

// Kubernetes-specific utility functions

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// PodReady returns true iff the pod is marked as ready (as determined by the pod's
// Status.Conditions)
func PodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}

// PodCompleted returns true iff all of the Pod's containers have stopped and will not be restarted
func PodCompleted(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// PodStartedBefore returns true iff Pod p started before Pod q
func PodStartedBefore(p, q *corev1.Pod) bool {
	return p.Status.StartTime.Before(q.Status.StartTime)
}

// TryPodOwnerVirtualMachine returns the name of the VirtualMachine that owns the pod, if there is
// one that does. Otherwise returns nil.
func TryPodOwnerVirtualMachine(pod *corev1.Pod) *NamespacedName {
	for _, ref := range pod.OwnerReferences {
		// For NeonVM, *at time of writing*, the OwnerReference has an APIVersion of
		// "vm.neon.tech/v1". But:
		//
		// 1. It's good to be extra-safe around possible name collisions for the
		//    "VirtualMachineMigration" name, even though *practically* it's not going to happen;
		// 2. We can disambiguate with the APIVersion; and
		// 3. We don't want to match on a fixed version, in case we want to change the version
		//    number later.
		//
		// So, given that the format is "<NAME>/<VERSION>", we can just match on the "<NAME>/" part
		// of the APIVersion to have the safety we want with the flexibility we need.
		if strings.HasPrefix(ref.APIVersion, "vm.neon.tech/") && ref.Kind == "VirtualMachine" {
			// note: OwnerReferences are not permitted to have a different namespace than the owned
			// object, so because VirtualMachineMigrations are namespaced, it must have the same
			// namespace as the Pod.
			return &NamespacedName{Namespace: pod.Namespace, Name: ref.Name}
		}
	}
	return nil
}

// TryPodOwnerVirtualMachineMigration returns the name of the VirtualMachineMigration that owns the
// pod, if there is one. Otherwise returns nil.
func TryPodOwnerVirtualMachineMigration(pod *corev1.Pod) *NamespacedName {
	for _, ref := range pod.OwnerReferences {
		if strings.HasPrefix(ref.APIVersion, "vm.neon.tech/") && ref.Kind == "VirtualMachineMigration" {
			return &NamespacedName{Namespace: pod.Namespace, Name: ref.Name}
		}
	}
	return nil
}
