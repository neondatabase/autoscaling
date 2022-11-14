package plugin

import (
	"context"
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
			msg := fmt.Sprintf(
				"[autscale-enforcer] Responding with status code %d to pod %v: %s",
				statusCode, req.Pod, err,
			)
			if 500 <= statusCode && statusCode < 600 {
				klog.Errorf("%s", msg)
			} else {
				klog.Warningf("%s", msg)
			}
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

	// Check whether the pod *will* migrate, then update its resources, and THEN start its
	// migration, using the possibly-changed resources.
	mustMigrate := e.updateMetricsAndCheckMustMigrate(pod, node, req.Metrics)
	permit, status, err := e.handleResources(pod, node, req.Resources, mustMigrate)
	if err != nil {
		return nil, status, err
	}

	var migrateDecision *api.MigrateResponse
	if mustMigrate {
		migrateDecision = &api.MigrateResponse{}
		err = e.state.startMigration(context.Background(), pod, e.virtClient)
		if err != nil {
			return nil, 500, fmt.Errorf("Error starting migration for pod %v: %s", pod.name, err)
		}
	}

	resp := api.PluginResponse{
		Permit: permit,
		Migrate: migrateDecision,
	}
	return &resp, 200, nil
}

// FIXME: this function has some duplicated sections. Ideally they wouldn't be (they don't have to!)
// but it's a little tricky to sus out how to merge the logic in a way that makes sense..
func (e *AutoscaleEnforcer) handleResources(
	pod *podState, node *nodeState, req api.Resources, startingMigration bool,
) (api.ResourcePermit, int, error) {
	oldNodeVCPUReserved := node.vCPU.reserved
	oldNodeVCPUPressure := node.vCPU.capacityPressure
	oldNodeVCPUPressureAccountedFor := node.vCPU.pressureAccountedFor
	oldPodVCPUReserved := pod.vCPU.reserved
	oldPodVCPUPressure := pod.vCPU.capacityPressure

	// Check that we aren't being asked to do something during migration:
	if pod.currentlyMigrating() {
		// The agent shouldn't have asked for a change after already receiving notice that it's
		// migrating.
		if req.VCPU != pod.vCPU.reserved {
			err := fmt.Errorf("cannot change vCPU: agent has already been informed that pod is migrating")
			return api.ResourcePermit{}, 400, err
		}
		return api.ResourcePermit{VCPU: pod.vCPU.reserved}, 200, nil
	} else if startingMigration {
		if req.VCPU > pod.vCPU.reserved {
			pod.vCPU.capacityPressure = req.VCPU - pod.vCPU.reserved
			node.vCPU.capacityPressure = node.vCPU.capacityPressure + pod.vCPU.capacityPressure - oldPodVCPUPressure
			// don't handle pressureAccountedFor here, that's taken care of in
			// (*pluginState).startMigration()
			klog.Infof(
				"[autoscale-enforcer] Denying pod %v vCPU increase %d -> %d because it is starting migration; node.vCPU.capacityPressure %d -> %d (%d -> %d spoken for)",
				pod.name, pod.vCPU.reserved, req.VCPU, oldNodeVCPUPressure, node.vCPU.capacityPressure, oldNodeVCPUPressureAccountedFor, node.vCPU.pressureAccountedFor,
			)
		} else /* `req.VCPU <= pod.vCPU.reserved` is implied */ {
			// Handle decrease-or-equal in the same way as below.
			node.vCPU.reserved -= pod.vCPU.reserved - req.VCPU
			pod.vCPU.reserved = req.VCPU
			pod.vCPU.capacityPressure = 0
			node.vCPU.capacityPressure -= oldPodVCPUPressure

			if oldPodVCPUPressure != 0 || req.VCPU != oldPodVCPUReserved {
				klog.Infof(
					"[autoscale-enforcer] Register pod %v vCPU %d -> %d (pressure %d -> %d); node.vCPU.reserved %d -> %d (of %d), node.vCPU.capacityPressure %d -> %d (%d -> %d spoken for)",
					pod.name, oldPodVCPUReserved, pod.vCPU.reserved, oldPodVCPUPressure, pod.vCPU.capacityPressure,
					oldNodeVCPUReserved, node.vCPU.reserved, node.totalReservableCPU(), oldNodeVCPUPressure, node.vCPU.capacityPressure, oldNodeVCPUPressureAccountedFor, node.vCPU.pressureAccountedFor,
				)
			}
		}

		return api.ResourcePermit{VCPU: pod.vCPU.reserved}, 200, nil
	}

	if req.VCPU <= pod.vCPU.reserved {
		// Decrease "requests" are actually just notifications it's already happened.
		node.vCPU.reserved -= pod.vCPU.reserved - req.VCPU
		pod.vCPU.reserved = req.VCPU
		// pressure is now zero, because the pod no longer wants to increase resources.
		pod.vCPU.capacityPressure = 0
		node.vCPU.capacityPressure -= oldPodVCPUPressure
	} else {
		increase := req.VCPU - pod.vCPU.reserved
		// Increases are bounded by what's left in the node:
		maxIncrease := node.remainingReservableCPU()
		if increase > maxIncrease /* increases are bounded by what's left in the node */ {
			pod.vCPU.capacityPressure = increase - maxIncrease
			// adjust node pressure accordingly. We can have old < new or new > old, so we shouldn't
			// directly += or -= (implicitly relying on overflow).
			node.vCPU.capacityPressure = node.vCPU.capacityPressure - oldPodVCPUPressure + pod.vCPU.capacityPressure
			increase = maxIncrease // cap at maxIncrease.
		} else {
			// If we're not capped by maxIncrease, relieve pressure coming from this pod
			node.vCPU.capacityPressure -= pod.vCPU.capacityPressure
			pod.vCPU.capacityPressure = 0
		}
		pod.vCPU.reserved += increase
		node.vCPU.reserved += increase
	}

	if oldPodVCPUReserved == req.VCPU && oldPodVCPUPressure == 0 {
		klog.Infof(
			"[autoscale-enforcer] Received request for pod %v staying at current vCPU count %d",
			pod.name, oldPodVCPUReserved,
		)
	} else {
		var wanted string
		if pod.vCPU.reserved != req.VCPU {
			wanted = fmt.Sprintf(" (wanted %d)", req.VCPU)
		}
		klog.Infof(
			"[autoscale-enforcer] Register pod %v vCPU %d -> %d%s (pressure %d -> %d); node.vCPU.reserved %d -> %d (of %d), node.vCPU.capacityPressure %d -> %d (%d -> %d spoken for)",
			pod.name, oldPodVCPUReserved, pod.vCPU.reserved, wanted, oldPodVCPUPressure, pod.vCPU.capacityPressure,
			oldNodeVCPUReserved, node.vCPU.reserved, node.totalReservableCPU(), oldNodeVCPUPressure, node.vCPU.capacityPressure, oldNodeVCPUPressureAccountedFor, node.vCPU.pressureAccountedFor,
		)
	}

	return api.ResourcePermit{VCPU: pod.vCPU.reserved}, 200, nil
}

func (e *AutoscaleEnforcer) updateMetricsAndCheckMustMigrate(
	pod *podState, node *nodeState, metrics api.Metrics,
) bool {
	// This pod should migrate if (a) we're looking for migrations and (b) it's next up in the
	// priority queue. We will give it a chance later to veto if the metrics have changed too much
	//
	// A third condition, "the pod is marked to always migrate" causes it to migrate even if neither
	// of the above conditions are met, so long as it has *previously* provided metrics.
	shouldMigrate := node.mq.isNextInQueue(pod) && node.tooMuchPressure()
	forcedMigrate := pod.testingOnlyAlwaysMigrate && pod.metrics != nil

	klog.Infof("[autoscale-enforcer] Updating pod %v metrics %+v -> %+v", pod.name, pod.metrics, metrics)
	oldMetrics := pod.metrics
	pod.metrics = &metrics
	if pod.currentlyMigrating() {
		return false // don't do anything else; it's already migrating.
	}

	node.mq.addOrUpdate(pod)

	if !shouldMigrate && !forcedMigrate {
		return false
	}

	// Give the pod a chance to veto migration if its metrics have significantly changed...
	var veto error
	if oldMetrics != nil && !forcedMigrate {
		veto = pod.checkOkToMigrate(*oldMetrics)
	}

	// ... but override the veto if it's still the best candidate anyways.
	stillFirst := node.mq.isNextInQueue(pod)

	if forcedMigrate || stillFirst || veto == nil {
		if veto != nil {
			klog.Infof("[autoscale-enforcer] Pod attempted veto of self migration, still highest-priority: %s", veto)
		}

		return true
	} else {
		klog.Infof("[autoscale-enforcer] Pod vetoed self migration: %s", veto)
		return false
	}
}
