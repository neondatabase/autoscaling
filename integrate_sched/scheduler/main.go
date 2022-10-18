package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/google/uuid"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	klog "k8s.io/klog/v2"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func main() {
	command := app.NewSchedulerCommand(app.WithPlugin("AutoscaleEnforcer", NewAutoscaleEnforcerPlugin))
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

type AutoscaleEnforcer struct {
	handle framework.Handle
	state  privateState
}

type podName struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type privateState struct {
	nodeMapLock sync.Mutex
	nodeMap     map[string]*nodeInfo
	podMapLock  sync.Mutex
	podMap      map[podName]*podInfo
}

type nodeInfo struct {
	maxCpu      uint16
	reservedCPU uint16 // of maxCpu, how much have we given to nodes?
	pods        map[podName]struct{}
}

type podInfo struct {
	node        string
	reservedCPU uint16
}

func (i *nodeInfo) totalReservableCPU() uint16 {
	return uint16(float32(i.maxCpu) * 0.8)
}

func (i *nodeInfo) remainingReservableCPU() uint16 {
	return i.totalReservableCPU() - i.reservedCPU
}

// NewAutoscaleEnforcerPlugin produces the initial AutoscaleEnforcer plugin to be used by the
// scheduler
func NewAutoscaleEnforcerPlugin(obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	// ^ obj can be used for taking in configuration. it's a bit tricky to figure out, and we don't
	// quite need it yet.
	klog.Info("[autoscale-enforcer] Initializing plugin")
	plugin := AutoscaleEnforcer{
		handle: h,
		state:  privateState{
			nodeMap: make(map[string]*nodeInfo),
			podMap: make(map[podName]*podInfo),
			// zero values are ok for the rest here
		},
	}
	go plugin.runPermitHandler()

	return &plugin, nil
}

// required for framework.Plugin
func (e *AutoscaleEnforcer) Name() string {
	return "AutoscaleEnforcer"
}

// required for framework.PreBind
//
// TODO: split into PreBind reserve vs PostBind commit
func (e *AutoscaleEnforcer) PreBind(
	ctx context.Context,
	state *framework.CycleState,
	p *corev1.Pod,
	nodeName string,
) *framework.Status {
	klog.V(1).Infof("[autoscale-enforcer] PreBind information: %s will bind to Node %s", p.Name, nodeName)

	info, err := e.getPodInfo(p)
	if err != nil {
		err = fmt.Errorf("bad metadata: %s", err)
		klog.Errorf("Skipping pod %s: %s", p.Name, err)
		return framework.NewStatus(
			framework.UnschedulableAndUnresolvable,
			err.Error(),
		)
	}

	// info.node is not set by getPodInfo
	info.node = nodeName

	podName := podName{
		Name:      p.Name,
		Namespace: p.Namespace,
	}
	e.state.podMapLock.Lock()
	e.state.podMap[podName] = info
	e.state.podMapLock.Unlock()

	e.state.nodeMapLock.Lock()
	node, ok := e.state.nodeMap[nodeName]
	if !ok {
		klog.V(1).Infof("[autoscale-enforcer] Fetching information for node %s")
		e.state.nodeMapLock.Unlock()
		node, err = e.getNodeInfo(ctx, nodeName)
		if err != nil {
			klog.Errorf("[autoscale-enforcer] PostBind: Error getting node info: %s", err)
			return framework.NewStatus(
				framework.Error,
				fmt.Sprintf("Error getting info for node %s", nodeName),
			)
		}
		e.state.nodeMapLock.Lock()
		e.state.nodeMap[nodeName] = node
	} else {
		klog.V(1).Infof("[autoscale-enforcer] Node information for %s already present")
	}

	// If there's capacity to reserve the pod, do that.
	if info.reservedCPU <= node.remainingReservableCPU() {
		newNodeReservedCPU := node.reservedCPU + info.reservedCPU
		klog.Infof(
			"[autoscale-enforcer] Allowing pod %s:%s (%d vCPU) in node %s: node.reservedCPU %d -> %d",
			podName.Namespace, podName.Name, info.reservedCPU, nodeName, node.reservedCPU, newNodeReservedCPU,
		)

		node.reservedCPU = newNodeReservedCPU
		node.pods[podName] = struct{}{} // add the pod to the set

		e.state.nodeMapLock.Unlock()

		return nil
	} else {
		err := fmt.Errorf(
			"Not enough available vCPU to reserve for pod init vCPU: need %d but have %d",
			info.reservedCPU, node.remainingReservableCPU(),
		)

		e.state.nodeMapLock.Unlock()

		klog.Errorf("[autoscale-enforcer] Can't schedule pod %s: %s", p.Name, err)
		return framework.NewStatus(
			framework.Unschedulable,
			err.Error(),
		)
	}
}

// The returned podInfo will not have podInfo.node set; that is for the caller.
func (e *AutoscaleEnforcer) getPodInfo(pod *corev1.Pod) (*podInfo, error) {
	// if this isn't a VM, it shouldn't have been scheduled with us
	if _, ok := pod.Labels["virtink.io/vm.name"]; !ok {
		return nil, fmt.Errorf("Pod is not a VM (missing virtink.io/vm.name label)")
	}

	cpuLabel := "autoscaler/init-cpu"
	initCpu, ok := pod.Labels[cpuLabel]
	if !ok {
		return nil, fmt.Errorf("VM missing label %q", cpuLabel)
	}

	initReservedCPU, err := strconv.ParseUint(initCpu, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("cannot parse pod label %q: %s", cpuLabel, err)
	}

	return &podInfo{reservedCPU: uint16(initReservedCPU)}, nil
}

func (e *AutoscaleEnforcer) getNodeInfo(ctx context.Context, nodeName string) (*nodeInfo, error) {
	node, err := e.handle.ClientSet().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	caps := node.Status.Capacity
	cpu := caps.Cpu()
	if cpu == nil {
		klog.Errorf("[autoscale-enforcer] PostBind: Node %s has no CPU capacity limit", nodeName)

		if node.Status.Allocatable.Cpu() != nil {
			klog.Warning(
				"[autoscale-enforcer] Using CPU allocatable limit as capacity for node %s",
				nodeName,
			)
			cpu = node.Status.Allocatable.Cpu()
		} else {
			return nil, fmt.Errorf("No CPU limits set")
		}
	}

	// Got CPU.
	maxCpu := cpu.MilliValue() / 1000 // cpu.Value rounds up. We don't want to do that.
	klog.V(1).Infof(
		"[autoscale-enforcer] Got CPU for node %s: %d total (milli = %d)",
		maxCpu, cpu.MilliValue(),
	)

	klog.Infof("[autoscale-enforcer] DEBUG: node %s max = %d, reserved = 0", nodeName, maxCpu)

	return &nodeInfo{
		maxCpu: uint16(maxCpu),
		reservedCPU: 0,
		pods: make(map[podName]struct{}),
	}, nil
}

// Duplicated from autoscaler-agent. TODO: extract into separate "types" library
type ResourceRequest struct {
	ID           uuid.UUID `json:"id"`
	VCPUs        uint16    `json:"vCPUs"`
	Pod          podName   `json:"pod"`
}

// Duplicated from autoscaler-agent.
type ResourceResponse struct {
	RequestID uuid.UUID `json:"requestId"`
	VCPUs     uint16    `json:"vCPUs"`
}

func (e *AutoscaleEnforcer) runPermitHandler() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(400)
			w.Write([]byte("must be POST"))
			return
		}

		var req ResourceRequest
		jsonDecoder := json.NewDecoder(r.Body)
		jsonDecoder.DisallowUnknownFields()
		if err := jsonDecoder.Decode(&req); err != nil {
			klog.Warningf("[autoscale-enforcer] Received bad JSON resource request: %s", err)
			w.WriteHeader(400)
			w.Write([]byte("bad JSON"))
			return
		}

		klog.Infof("[autoscale-enforcer] Received resource request %+v", req)
		// TODO: have something a bit more robust here...

		// Get pod & node information
		e.state.podMapLock.Lock()
		defer e.state.podMapLock.Unlock()

		pod, ok := e.state.podMap[req.Pod]
		if !ok {
			klog.Warningf("[autoscale-enforcer] Received request for pod we don't know: %+v", req)
			w.WriteHeader(404)
			w.Write([]byte("pod not found"))
			return
		}

		e.state.nodeMapLock.Lock()
		defer e.state.nodeMapLock.Unlock()

		node := e.state.nodeMap[pod.node]

		oldNodeCPU := node.reservedCPU
		oldPodCPU := pod.reservedCPU

		if req.VCPUs <= pod.reservedCPU {
			// Decrease "requests" are actually just notifications it's already happened.
			node.reservedCPU -= pod.reservedCPU - req.VCPUs
			pod.reservedCPU = req.VCPUs
		} else {
			// Increases are bounded by what's left in the node:
			increase := req.VCPUs - pod.reservedCPU
			maxIncrease := node.remainingReservableCPU()
			if increase > maxIncrease {
				increase = maxIncrease
			}

			node.reservedCPU += increase
			pod.reservedCPU += increase
		}

		klog.Infof(
			"[autoscale-enforcer] Register pod vCPU change %d -> %d for %s:%s; node.reservedCPU %d -> %d (of %d)",
			oldPodCPU, pod.reservedCPU, req.Pod.Namespace, req.Pod.Name, oldNodeCPU, node.reservedCPU, node.totalReservableCPU(),
		)

		resp := ResourceResponse{
			RequestID: req.ID,
			VCPUs:     pod.reservedCPU, // We've updated reservedCPU, so this is the amount it's ok to hand out
		}

		responseBody, err := json.Marshal(&resp)
		if err != nil {
			klog.Fatalf("[autoscale-enforcer] Error encoding response JSON: %s", err)
		}

		klog.Infof("[autoscale-enforcer] Responding with %+v to pod %s", resp, req.Pod)
		w.WriteHeader(200)
		w.Write(responseBody)
		return
	})
	server := http.Server{Addr: "0.0.0.0:10299", Handler: mux}
	klog.Info("[autoscale-enforcer] Starting resource request server")
	klog.Fatalf("[autoscale-enforcer] Resource request server failed: %s", server.ListenAndServe())
}
