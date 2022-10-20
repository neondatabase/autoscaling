package plugin

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	klog "k8s.io/klog/v2"
)

// Duplicated from autoscaler-agent. TODO: extract into separate "types" library
type resourceRequest struct {
	ID    uuid.UUID `json:"id"`
	VCPUs uint16    `json:"vCPUs"`
	Pod   podName   `json:"pod"`
}

// Duplicated from autoscaler-agent.
type permit struct {
	RequestID uuid.UUID `json:"requestId"`
	VCPUs     uint16    `json:"vCPUs"`
}

// runPermitHandler runs the server for handling each resourceRequest from a pod
func (e *AutoscaleEnforcer) runPermitHandler() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(400)
			w.Write([]byte("must be POST"))
			return
		}

		var req resourceRequest
		jsonDecoder := json.NewDecoder(r.Body)
		jsonDecoder.DisallowUnknownFields()
		if err := jsonDecoder.Decode(&req); err != nil {
			klog.Warningf("[autoscale-enforcer] Received bad JSON resource request: %s", err)
			w.WriteHeader(400)
			w.Write([]byte("bad JSON"))
			return
		}

		klog.Infof("[autoscale-enforcer] Received resource request %+v", req)
		body, contentType, statusCode := e.handleResourceRequest(req)
		if contentType != "" {
			w.Header().Add("Content-Type", contentType)
		}
		w.WriteHeader(statusCode)
		w.Write(body)
		return
	})
	server := http.Server{Addr: "0.0.0.0:10299", Handler: mux}
	klog.Info("[autoscale-enforcer] Starting resource request server")
	klog.Fatalf("[autoscale-enforcer] Resource request server failed: %s", server.ListenAndServe())
}

// handleResourceRequest handles a single resourceRequest
//
// Returns response body, Content-Type header, and status code.
func (e *AutoscaleEnforcer) handleResourceRequest(req resourceRequest) ([]byte, string, int) {
	locked := true
	e.state.lock.Lock()
	defer func() {
		if locked {
			e.state.lock.Unlock()
		}
	}()

	pod, ok := e.state.podMap[req.Pod]
	if !ok {
		klog.Warningf("[autoscale-enforcer] Received request for pod we don't know: %+v", req)
		return []byte("pod not found"), "", 404
	}

	node := pod.node

	oldNodeCPU := node.reservedCPU
	oldPodCPU := pod.reservedCPU

	if req.VCPUs < pod.reservedCPU {
		// Decrease "requests" are actually just notifications it's already happened.
		node.reservedCPU -= pod.reservedCPU - req.VCPUs
		pod.reservedCPU = req.VCPUs
	} else if req.VCPUs > pod.reservedCPU {
		// Increases are bounded by what's left in the node:
		increase := req.VCPUs - pod.reservedCPU
		maxIncrease := node.remainingReservableCPU()
		if increase > maxIncrease {
			increase = maxIncrease
		}

		node.reservedCPU += increase
		pod.reservedCPU += increase
	}

	if oldPodCPU == req.VCPUs {
		klog.Warningf(
			"[autoscale-enforcer] Received request for pod %v for current vCPU count %d",
			req.Pod, oldPodCPU,
		)
	} else if oldPodCPU != pod.reservedCPU {
		klog.Infof(
			"[autoscale-enforcer] Register pod %v vCPU change %d -> %d; node.reservedCPU %d -> %d (of %d)",
			req.Pod, oldPodCPU, pod.reservedCPU, oldNodeCPU, node.reservedCPU, node.totalReservableCPU(),
		)
	} else {
		klog.Infof(
			"[autoscale-enforcer] Denied pod %v vCPU change %d -> %d, remain at %d; node.reservedCPU = %d of %d",
			req.Pod, oldPodCPU, req.VCPUs, pod.reservedCPU, node.reservedCPU, node.totalReservableCPU(),
		)
	}

	resp := permit{
		RequestID: req.ID,
		VCPUs:     pod.reservedCPU, // We've updated reservedCPU, so this is the amount it's ok to hand out
	}

	// We're done *handling* this request, so we can release the lock early.
	locked = false
	e.state.lock.Unlock()

	responseBody, err := json.Marshal(&resp)
	if err != nil {
		klog.Fatalf("[autoscale-enforcer] Error encoding response JSON: %s", err)
	}

	klog.Infof("[autoscale-enforcer] Responding to pod %v with %+v", req.Pod, resp)
	return responseBody, "application/json", 200
}
