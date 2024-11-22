package util

// Kubernetes-specific utility functions

import (
	corev1 "k8s.io/api/core/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
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

func azForTerm(term corev1.NodeSelectorTerm) string {
	for _, expr := range term.MatchExpressions {
		isAZ := expr.Key == "topology.kubernetes.io/zone" &&
			expr.Operator == corev1.NodeSelectorOpIn &&
			len(expr.Values) == 1
		if isAZ {
			return expr.Values[0]
		}
	}

	return ""
}

// PodPreferredAZIfPresent returns the desired availability zone of the Pod, if it has one
func PodPreferredAZIfPresent(pod *corev1.Pod) string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return ""
	}

	affinity := pod.Spec.Affinity.NodeAffinity

	// First, check required affinities for AZ:
	if affinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if az := azForTerm(term); az != "" {
				return az
			}
		}
	}

	// Then, check preferred:
	for _, term := range affinity.PreferredDuringSchedulingIgnoredDuringExecution {
		if az := azForTerm(term.Preference); az != "" {
			return az
		}
	}

	// no AZ present
	return ""
}

// TryPodOwnerVirtualMachine returns the name of the VirtualMachine that owns the pod, if there is
// one that does. Otherwise returns nil.
func TryPodOwnerVirtualMachine(pod *corev1.Pod) *NamespacedName {
	ref, ok := vmv1.VirtualMachineOwnerForPod(pod)
	if !ok {
		return nil
	}

	// note: OwnerReferences are not permitted to have a different namespace than the owned
	// object, so because VirtualMachineMigrations are namespaced, it must have the same
	// namespace as the Pod.
	return &NamespacedName{Namespace: pod.Namespace, Name: ref.Name}
}

// TryPodOwnerVirtualMachineMigration returns the name of the VirtualMachineMigration that owns the
// pod, if there is one. Otherwise returns nil.
func TryPodOwnerVirtualMachineMigration(pod *corev1.Pod) *NamespacedName {
	ref, _, ok := vmv1.MigrationOwnerForPod(pod)
	if !ok {
		return nil
	}

	return &NamespacedName{Namespace: pod.Namespace, Name: ref.Name}
}
