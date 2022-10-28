package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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
		var req api.AgentRequest
		jsonDecoder := json.NewDecoder(io.LimitReader(r.Body, MaxHTTPBodySize))
		jsonDecoder.DisallowUnknownFields()
		if err := jsonDecoder.Decode(&req); err != nil {
			klog.Warningf("[autoscale-enforcer] Received bad JSON request: %s", err)
			w.Header().Add("Content-Type", ContentTypeError)
			w.WriteHeader(400)
			w.Write([]byte("bad JSON"))
			return
		}

		klog.Infof(
			"[autoscale-enforcer] Received autoscaler-agent request (client = %s) %+v",
			r.RemoteAddr, req,
		)
		resp, statusCode, err := e.handleAgentRequest(req)
		if err != nil {
			w.Header().Add("Content-Type", ContentTypeError)
			w.WriteHeader(statusCode)
			w.Write([]byte(err.Error()))
			return
		}

		responseBody, err := json.Marshal(&resp)
		if err != nil {
			klog.Fatalf("Error encoding response JSON: %s", err)
		}

		w.Header().Add("Content-Type", ContentTypeJSON)
		w.WriteHeader(statusCode)
		w.Write(responseBody)
		return
	})
	server := http.Server{Addr: "0.0.0.0:10299", Handler: mux}
	klog.Info("[autoscale-enforcer] Starting resource request server")
	klog.Fatalf("[autoscale-enforcer] Resource request server failed: %s", server.ListenAndServe())
}

// Returns body (if successful), status code, error (if unsuccessful)
func (e *AutoscaleEnforcer) handleAgentRequest(req api.AgentRequest) (*api.PluginResponse, int, error) {
	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[req.Pod]
	if !ok {
		klog.Warningf("[autoscale-enforcer] Received request for pod we don't know: %+v", req)
		return nil, 404, fmt.Errorf("pod not found")
	}

	node := pod.node

	//////////////////////////////////////
	// Handle api.ResourceRequest piece //
	//////////////////////////////////////

	oldNodeVCPUReserved := node.vCPU.reserved
	oldNodeVCPUPressure := node.vCPU.pressure
	oldNodeVCPUPressureAccountedFor := node.vCPU.pressureAccountedFor
	oldPodVCPUReserved := pod.vCPU.reserved
	oldPodVCPUPressure := pod.vCPU.pressure

	if req.Resources.VCPU <= pod.vCPU.reserved {
		// Decrease "requests" are actually just notifications it's already happened.
		node.vCPU.reserved -= pod.vCPU.reserved - req.Resources.VCPU
		pod.vCPU.reserved = req.Resources.VCPU
		// pressure is now zero, because the pod no longer wants to increase resources.
		pod.vCPU.pressure = 0
		node.vCPU.pressure -= oldPodVCPUPressure
		if pod.currentlyMigrating() {
			node.vCPU.pressureAccountedFor -= oldPodVCPUPressure + (oldPodVCPUReserved - pod.vCPU.reserved)
		}
	} else {
		increase := req.Resources.VCPU - pod.vCPU.reserved
		// Increases are bounded by what's left in the node:
		maxIncrease := node.remainingReservableCPU()
		if increase > maxIncrease /* increases are bounded by what's left in the node */ {
			pod.vCPU.pressure = increase - maxIncrease
			// adjust node pressure accordingly. We can have old < new or new > old, so we shouldn't
			// directly += or -= (implicitly relying on overflow).
			node.vCPU.pressure = node.vCPU.pressure - oldPodVCPUPressure + pod.vCPU.pressure
			if pod.currentlyMigrating() {
				node.vCPU.pressureAccountedFor = node.vCPU.pressureAccountedFor - oldPodVCPUPressure + pod.vCPU.pressure
			}
			increase = maxIncrease // cap at maxIncrease.
		} else {
			// If we're not capped by maxIncrease, relieve pressure coming from this pod
			if pod.currentlyMigrating() {
				node.vCPU.pressureAccountedFor -= pod.vCPU.pressure
			}
			node.vCPU.pressure -= pod.vCPU.pressure
			pod.vCPU.pressure = 0
		}
		pod.vCPU.reserved += increase
		node.vCPU.reserved += increase
		if pod.currentlyMigrating() {
			node.vCPU.pressureAccountedFor += increase
		}
	}

	if oldPodVCPUReserved == req.Resources.VCPU {
		klog.Infof(
			"[autoscale-enforcer] Received request for pod %v staying at current vCPU count %d",
			req.Pod, oldPodVCPUReserved,
		)
	} else {
		var wanted string
		if pod.vCPU.reserved != req.Resources.VCPU {
			wanted = fmt.Sprintf(" (wanted %d)", req.Resources.VCPU)
		}
		klog.Infof(
			"[autoscale-enforcer] Register pod %v vCPU %d -> %d%s (pressure %d -> %d); node.vCPU.reserved %d -> %d (of %d), node.vCPU.pressure %d -> %d (%d -> %d spoken for)",
			pod.name, oldPodVCPUReserved, pod.vCPU.reserved, wanted, oldPodVCPUPressure, pod.vCPU.pressure,
			oldNodeVCPUReserved, node.vCPU.reserved, node.totalReservableCPU(), oldNodeVCPUPressure, node.vCPU.pressure, oldNodeVCPUPressureAccountedFor, node.vCPU.pressureAccountedFor,
		)
	}

	permit := api.ResourcePermit{VCPU: pod.vCPU.reserved}

	/////////////////////////////////////
	// Handle api.MetricsRequest piece //
	/////////////////////////////////////

	// This pod should migrate if (a) we're looking for migrations and (b) it's next up in the
	// priority queue. We will give it a chance later to veto if the metrics have changed too much
	shouldMigrate := node.mq.isNextInQueue(pod) && node.tooMuchPressure()

	klog.Infof("[autoscale-enforcer] Updating pod %v metrics %+v -> %+v", pod.name, pod.metrics, req.Metrics)
	oldMetrics := pod.metrics
	pod.metrics = &req.Metrics
	if !pod.currentlyMigrating() {
		node.mq.addOrUpdate(pod)
	}

	var migrateDecision *api.MigrateResponse

	if shouldMigrate /* <- will be false if currently migrating */ {

		// Give the pod a chance to veto migration if its metrics have significantly changed...
		var veto error
		if oldMetrics != nil {
			veto = pod.checkOkToMigrate(*oldMetrics)
		}

		// ... but override the veto if it's still the best candidate anyways.
		stillFirst := node.mq.isNextInQueue(pod)

		if stillFirst || veto == nil {
			if veto != nil {
				klog.Infof("[autoscale-enforcer] Pod attempted veto of self migration, still highest-priority: %s", veto)
			}
			klog.Infof("[autoscale-enforcer] Starting migration for pod %v", pod.name)

			var err error
			migrateDecision, err = e.startMigration(pod)
			if err != nil {
				klog.Errorf("[autoscale-enforcer] Error starting migration for pod %v: %s", pod.name, err)
				return nil, 500, err
			}

			// TODO: we'll add more stuff to this as the feature gets fleshed out
			migrateDecision = &api.MigrateResponse{}
		} else {
			klog.Infof("[autoscale-enforcer] Pod vetoed self migration: %s", veto)
		}
	}

	resp := api.PluginResponse{
		Permit: permit,
		Migrate: migrateDecision,
	}
	return &resp, 200, nil
}

// startMigration starts the VM migration process for a single pod, returning the information it
// needs
//
// This method assumes that the caller currently has a lock on the state.
func (e *AutoscaleEnforcer) startMigration(pod *podState) (*api.MigrateResponse, error) {
	if pod.currentlyMigrating() {
		return nil, fmt.Errorf("Pod is already migrating: state = %+v", pod.migrationState)
	}

	// Remove the pod from the migration queue.
	pod.node.mq.removeIfPresent(pod)
	
	// TODO: currently this is just a skeleton of the implementation; we'll fill this out more with
	// actual functionality later.
	pod.migrationState = &podMigrationState{}
	oldNodeVCPUPressureAccountedFor := pod.node.vCPU.pressureAccountedFor
	pod.node.vCPU.pressureAccountedFor += pod.vCPU.reserved + pod.vCPU.pressure

	klog.Infof(
		"[autoscaler-enforcer] Migrate pod %v; node.vCPU.pressure = %d (%d -> %d spoken for)",
		pod.name, pod.node.vCPU.pressure, oldNodeVCPUPressureAccountedFor, pod.node.vCPU.pressureAccountedFor,
	)

	return &api.MigrateResponse{}, nil
}
