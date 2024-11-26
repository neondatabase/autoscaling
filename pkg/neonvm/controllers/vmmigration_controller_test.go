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
		node   *corev1.Node
	}{
		{
			name:   "No architecture affinity",
			result: false,
			vmSpec: &vmv1.VirtualMachine{},
			node:   &corev1.Node{},
		},
		{
			name:   "VM spec has affinity",
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
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{
						Architecture: "amd64",
					},
				},
			},
		},
		{
			name:   "VM affinity and node has different architectures",
			result: false,
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
												Values:   []string{"arm64"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{
						Architecture: "amd64",
					},
				},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := hasArchitectureAffinity(testCase.vmSpec, testCase.node)
			if result != testCase.result {
				t.Errorf("Expected %v, got %v", testCase.result, result)
			}
		})
	}
}

func TestAddArchitectureAffinity(t *testing.T) {
	vmSpec := &vmv1.VirtualMachine{}
	sourceNode := &corev1.Node{
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				Architecture: "amd64",
			},
		},
	}
	if hasArchitectureAffinity(vmSpec, sourceNode) {
		t.Errorf("Expected no architecture affinity")
	}
	if err := addArchitectureAffinity(vmSpec, &corev1.Node{
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				Architecture: "amd64",
			},
		},
	}); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if !hasArchitectureAffinity(vmSpec, sourceNode) {
		t.Errorf("Expected architecture affinity to be added")
	}
}

func TestSourceNodeHasNoArchitecture(t *testing.T) {
	sourceNode := &corev1.Node{
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				Architecture: "",
			},
		},
	}

	vmSpec := &vmv1.VirtualMachine{}

	err := addArchitectureAffinity(vmSpec, sourceNode)
	if err == nil {
		t.Errorf("Expected error when source node has no architecture")
	}
}
