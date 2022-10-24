package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
)

var MaxHTTPBodySize int64 = 1 << 10 // 1 KiB
var ContentTypeJSON string = "application/json"
var ContentTypeError string = "text/plain"

// runPermitHandler runs the server for handling each resourceRequest from a pod
func (e *AutoscaleEnforcer) runPermitHandler() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(400)
			w.Write([]byte("must be POST"))
			return
		}

		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxHTTPBodySize))
		if err != nil {
			klog.Warningf("[autoscale-enforcer] Error reading HTTP body: %s", err)
		}

		var msg api.AgentMessage
		if err = json.Unmarshal(body, &msg); err != nil {
			klog.Warningf("[autoscale-enforcer] Received bad JSON request: %s", err)
			w.WriteHeader(400)
			w.Write([]byte("bad JSON"))
			return
		}

		klog.Infof("[autoscale-enforcer] Received autoscaler-agent message %+v", msg)
		body, contentType, statusCode := e.handleAgentMessage(msg)
		w.Header().Add("Content-Type", contentType)
		w.WriteHeader(statusCode)
		w.Write(body)
		return
	})
	server := http.Server{Addr: "0.0.0.0:10299", Handler: mux}
	klog.Info("[autoscale-enforcer] Starting resource request server")
	klog.Fatalf("[autoscale-enforcer] Resource request server failed: %s", server.ListenAndServe())
}

// Returns response body, Content-Type header, and status code.
func (e *AutoscaleEnforcer) handleAgentMessage(msg api.AgentMessage) ([]byte, string, int) {
	msgKind := msg.Kind()
	switch msgKind {
	case api.MessageKindResource:
		req, err := msg.AsResourceRequest()
		if err != nil {
			klog.Errorf("[autoscale-enforcer] message has kind %q but casting failed: %s", msgKind, err)
			return []byte("internal error"), ContentTypeError, 500
		}

		resp, status, err := e.handleResourceRequest(msg.ID, req)
		if err != nil {
			return []byte(err.Error()), ContentTypeError, status
		}
		klog.Infof("[autoscale-enforcer] Responding to pod %v with %+v", req.Pod, resp)

		responseBody, err := json.Marshal(resp)
		if err != nil {
			klog.Fatalf("[autoscale-enforcer] Error encoding response JSON: %s", err)
		}
		return responseBody, ContentTypeJSON, 200
	default:
		klog.Errorf("[autoscale-enforcer] Unexpected autoscaler-agent message kind %q", msgKind)
		return []byte("internal error"), ContentTypeError, 500
	}
}

// handleResourceRequest handles a single api.ResourceRequest
func (e *AutoscaleEnforcer) handleResourceRequest(
	requestID uuid.UUID,
	req *api.ResourceRequest,
) (*api.PluginMessage, int, error) {
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
		return nil, 404, fmt.Errorf("pod not found")
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

	// We're done *handling* this request, so we can release the lock early.
	locked = false
	e.state.lock.Unlock()

	// We've updated reservedCPU, so this is the amount it's ok to hand out
	permit := api.ResourcePermit{VCPUs: pod.reservedCPU}
	resp := permit.Response(requestID)
	return &resp, 200, nil
}
