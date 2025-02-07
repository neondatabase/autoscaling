package main

import (
	"encoding/json"
	"fmt"

	"github.com/samber/lo"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type TraceEvent struct {
	// Timestamp is the time at which this event was observed.
	Timestamp metav1.Time `json:"ts"`

	EventKind EventKind `json:"eventKind"`

	ObjectKind string `json:"objectKind"`

	// Name is the (potentially namespaced) name of the object. If the object *is* namespaced, this
	// field has the format "<namespace>/<name>". Otherwise it's just the object's metadata.name.
	Name string `json:"name"`
	// UID is the value of the object's metadata.uid field.
	UID string `json:"uid"`

	// Labels is the subset of the object's labels that were configured to be preserved in the events
	Labels map[string]string `json:"labels"`

	// Preexisting, if present, indicates whether the object in a Created event already existed
	// within the cluster (which will be the case on initial startup).
	Preexisting *bool `json:"preexisting,omitempty"`

	// DeletionRequested is true iff the object's metadata.deletionTimestamp field is set.
	DeletionRequested bool `json:"deletionRequested"`

	*ErrorData
	*PodData
	*NodeData
}

type EventKind string

const (
	EventCreated  EventKind = "Created"
	EventModified EventKind = "Modified"
	EventDeleted  EventKind = "Deleted"
	EventError    EventKind = "Error"
)

type ErrorData struct {
	Error string `json:"error"`
}

type PodData struct {
	// NodeName is the node that the Pod has been scheduled onto, if it has been scheduled.
	NodeName string `json:"nodeName"`

	// IsVirtualMachine is true iff the
	IsVirtualMachine bool `json:"IsVirtualMachine"`

	// Resources gives the requested resources of the pod (if not a VM), or the VM's current
	// requested CPU and memory amounts (if a VM).
	Resources vmv1.VirtualMachineUsage `json:"resources"`

	Priority                  *int32                            `json:"priority"`
	Affinity                  *corev1.Affinity                  `json:"affinity"`
	NodeSelector              map[string]string                 `json:"nodeSelector"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints"`
	Tolerations               []corev1.Toleration               `json:"tolerations"`

	*VirtualMachinePodData
}

type VirtualMachinePodData struct {
	// SpecResourceBounds give the values of the VM's resource bounds, as configured by in the spec,
	// for absolute limits on the size of the VM.
	SpecResourceBounds vmv1.VirtualMachineResources `json:"SpecResourceBounds"`

	AutoscalingEnabled bool `json:"autoscalingEnabled"`

	// AutoscalingBounds give the resource bounds specified in the autoscaling bounds annotation, if
	// autoscaling is enabled and the annotation is present.
	AutoscalingBounds *api.ScalingBounds `json:"autoscalingBounds"`
}

func ExtractPodData(pod *corev1.Pod) (*PodData, error) {
	_, isVM := vmv1.VirtualMachineOwnerForPod(pod)

	var resources vmv1.VirtualMachineUsage
	var vmData *VirtualMachinePodData

	if !isVM {
		cpu := new(resource.Quantity)
		mem := new(resource.Quantity)

		for _, container := range pod.Spec.Containers {
			cpu.Add(*container.Resources.Requests.Cpu())
			mem.Add(*container.Resources.Requests.Memory())
		}

		resources = vmv1.VirtualMachineUsage{CPU: cpu, Memory: mem}
	} else {
		var err error
		vmData, resources, err = extractVirtualMachinePodData(pod)
		if err != nil {
			return nil, err
		}
	}

	return &PodData{
		NodeName:                  pod.Spec.NodeName,
		IsVirtualMachine:          isVM,
		Resources:                 resources,
		Priority:                  pod.Spec.Priority,
		Affinity:                  pod.Spec.Affinity,
		NodeSelector:              pod.Spec.NodeSelector,
		TopologySpreadConstraints: pod.Spec.TopologySpreadConstraints,
		Tolerations:               pod.Spec.Tolerations,
		VirtualMachinePodData:     vmData,
	}, nil
}

func extractVirtualMachinePodData(
	pod *corev1.Pod,
) (*VirtualMachinePodData, vmv1.VirtualMachineUsage, error) {
	usage, err := vmv1.VirtualMachineUsageFromPod(pod)
	if err != nil {
		usage := lo.Empty[vmv1.VirtualMachineUsage]()
		return nil, usage, fmt.Errorf("failed to get VirtualMachine usage from annotation: %w", err)
	}

	specBounds, err := vmv1.VirtualMachineResourcesFromPod(pod)
	if err != nil {
		return nil, *usage, fmt.Errorf("failed to get VirtualMachine spec resources from annotation: %w", err)
	}

	autoscalingEnabled := api.HasAutoscalingEnabled(pod)

	var autoscalingBounds *api.ScalingBounds
	if autoscalingEnabled {
		if ann, ok := pod.Annotations[api.AnnotationAutoscalingBounds]; ok {
			autoscalingBounds = new(api.ScalingBounds)
			if err := json.Unmarshal([]byte(ann), autoscalingBounds); err != nil {
				return nil, *usage, fmt.Errorf("failed to parse autoscaling bounds annotation: %w", err)
			}
		}
	}

	data := &VirtualMachinePodData{
		SpecResourceBounds: *specBounds,
		AutoscalingEnabled: autoscalingEnabled,
		AutoscalingBounds:  autoscalingBounds,
	}

	return data, *usage, nil
}

type NodeData struct {
	AllocatableResources vmv1.VirtualMachineUsage `json:"allocatableResources"`

	// Unschedulable gives the value of the Node's spec.unschedulable field, which is set to true
	// when the node is cordoned.
	Unschedulable bool `json:"unschedulable"`
	// Ready is true iff the node has a "Ready" condition set to True.
	Ready bool `json:"ready"`
}

func ExtractNodeData(node *corev1.Node) *NodeData {
	var ready bool
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" {
			ready = condition.Status == "True"
			break
		}
	}

	return &NodeData{
		AllocatableResources: vmv1.VirtualMachineUsage{
			CPU:    node.Status.Allocatable.Cpu(),
			Memory: node.Status.Allocatable.Memory(),
		},
		Unschedulable: node.Spec.Unschedulable,
		Ready:         ready,
	}
}
