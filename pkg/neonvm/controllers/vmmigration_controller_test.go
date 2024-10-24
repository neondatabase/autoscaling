package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

func TestHasArchitectureAffinity(t *testing.T) {

	cases := []struct {
		name   string
		result bool
		vmSpec *vmv1.VirtualMachine
	}{
		{
			name:   "No architecture affinity",
			result: false,
			vmSpec: &vmv1.VirtualMachine{},
		},
		{
			name:   "Architecture affinity",
			result: true,
			vmSpec: &vmv1.VirtualMachine{
				Spec: vmv1.VirtualMachineSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "kubernetes.io/arch",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"amd64"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := hasArchitectureAffinity(testCase.vmSpec)
			if result != testCase.result {
				t.Errorf("Expected %v, got %v", testCase.result, result)
			}
		})
	}
}

func TestAddArchitectureAffinity(t *testing.T) {
	vmSpec := &vmv1.VirtualMachine{}
	if hasArchitectureAffinity(vmSpec) {
		t.Errorf("Expected no architecture affinity")
	}
	addArchitectureAffinity(vmSpec, &corev1.Node{
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				Architecture: "amd64",
			},
		},
	})

	if !hasArchitectureAffinity(vmSpec) {
		t.Errorf("Expected architecture affinity to be added")
	}
}
