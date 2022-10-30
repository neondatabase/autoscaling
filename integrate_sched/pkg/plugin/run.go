package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	klog "k8s.io/klog/v2"
	ktypes "k8s.io/apimachinery/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	migrateDecision, status, err := e.handleMetricsAndMigration(pod, node, req.Metrics)
	if err != nil {
		return nil, status, err
	}
	permit, status, err := e.handleResources(pod, node, req.Resources, migrateDecision != nil)
	if err != nil {
		return nil, status, err
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
	oldNodeVCPUPressure := node.vCPU.pressure
	oldNodeVCPUPressureAccountedFor := node.vCPU.pressureAccountedFor
	oldPodVCPUReserved := pod.vCPU.reserved
	oldPodVCPUPressure := pod.vCPU.pressure

	// Check that we aren't being asked to do something during migration:
	if pod.currentlyMigrating() {
		// The agent shouldn't have asked for a change after already receiving notice that it's
		// migrating.
		if req.VCPU != pod.vCPU.reserved && !startingMigration {
			err := fmt.Errorf("cannot change vCPU: agent has already been informed that pod is migrating")
			return api.ResourcePermit{}, 400, err
		}

		if req.VCPU > pod.vCPU.reserved /* `&& startingMigration` is implied */ {
			pod.vCPU.pressure = req.VCPU - pod.vCPU.reserved
			node.vCPU.pressure = node.vCPU.pressure + pod.vCPU.pressure - oldPodVCPUPressure
			node.vCPU.pressureAccountedFor = node.vCPU.pressureAccountedFor + pod.vCPU.pressure - oldPodVCPUPressure
			klog.Infof(
				"[autoscale-enforcer] Denying pod %v vCPU increase %d -> %d because it is starting migration; node.vCPU.pressure %d -> %d (%d -> %d spoken for)",
				pod.name, pod.vCPU.reserved, req.VCPU, oldNodeVCPUPressure, node.vCPU.pressure, oldNodeVCPUPressureAccountedFor, node.vCPU.pressureAccountedFor,
			)
		} else /* `req.VCPU <= pod.vCPU.reserved && startingMigration` is implied */ {
			// Handle decrease-or-equal in the same way as below.
			node.vCPU.reserved -= pod.vCPU.reserved - req.VCPU
			pod.vCPU.reserved = req.VCPU
			pod.vCPU.pressure = 0
			node.vCPU.pressure -= oldPodVCPUPressure

			if oldPodVCPUPressure != 0 || req.VCPU != oldPodVCPUReserved {
				klog.Infof(
					"[autoscale-enforcer] Register pod %v vCPU %d -> %d (pressure %d -> %d); node.vCPU.reserved %d -> %d (of %d), node.vCPU.pressure %d -> %d (%d -> %d spoken for)",
					pod.name, oldPodVCPUReserved, pod.vCPU.reserved, oldPodVCPUPressure, pod.vCPU.pressure,
					oldNodeVCPUReserved, node.vCPU.reserved, node.totalReservableCPU(), oldNodeVCPUPressure, node.vCPU.pressure, oldNodeVCPUPressureAccountedFor, node.vCPU.pressureAccountedFor,
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
		pod.vCPU.pressure = 0
		node.vCPU.pressure -= oldPodVCPUPressure
	} else {
		increase := req.VCPU - pod.vCPU.reserved
		// Increases are bounded by what's left in the node:
		maxIncrease := node.remainingReservableCPU()
		if increase > maxIncrease /* increases are bounded by what's left in the node */ {
			pod.vCPU.pressure = increase - maxIncrease
			// adjust node pressure accordingly. We can have old < new or new > old, so we shouldn't
			// directly += or -= (implicitly relying on overflow).
			node.vCPU.pressure = node.vCPU.pressure - oldPodVCPUPressure + pod.vCPU.pressure
			increase = maxIncrease // cap at maxIncrease.
		} else {
			// If we're not capped by maxIncrease, relieve pressure coming from this pod
			node.vCPU.pressure -= pod.vCPU.pressure
			pod.vCPU.pressure = 0
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
			"[autoscale-enforcer] Register pod %v vCPU %d -> %d%s (pressure %d -> %d); node.vCPU.reserved %d -> %d (of %d), node.vCPU.pressure %d -> %d (%d -> %d spoken for)",
			pod.name, oldPodVCPUReserved, pod.vCPU.reserved, wanted, oldPodVCPUPressure, pod.vCPU.pressure,
			oldNodeVCPUReserved, node.vCPU.reserved, node.totalReservableCPU(), oldNodeVCPUPressure, node.vCPU.pressure, oldNodeVCPUPressureAccountedFor, node.vCPU.pressureAccountedFor,
		)
	}

	return api.ResourcePermit{VCPU: pod.vCPU.reserved}, 200, nil
}

func (e *AutoscaleEnforcer) handleMetricsAndMigration(
	pod *podState, node *nodeState, metrics api.Metrics,
) (*api.MigrateResponse, int, error) {
	// This pod should migrate if (a) we're looking for migrations and (b) it's next up in the
	// priority queue. We will give it a chance later to veto if the metrics have changed too much
	shouldMigrate := node.mq.isNextInQueue(pod) && node.tooMuchPressure()

	klog.Infof("[autoscale-enforcer] Updating pod %v metrics %+v -> %+v", pod.name, pod.metrics, metrics)
	oldMetrics := pod.metrics
	pod.metrics = &metrics
	if pod.currentlyMigrating() {
		return nil, 200, nil // don't do anything else; it's already migrating.
	}

	node.mq.addOrUpdate(pod)

	if !shouldMigrate {
		return nil, 200, nil;
	}

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

		resp, err := e.startMigration(context.Background(), pod)
		if err != nil {
			klog.Errorf("[autoscale-enforcer] Error starting migration for pod %v: %s", pod.name, err)
			return nil, 500, err
		}

		// TODO: we'll add more stuff to this as the feature gets fleshed out
		return resp, 200, nil
	} else {
		klog.Infof("[autoscale-enforcer] Pod vetoed self migration: %s", veto)
		return nil, 200, nil
	}
}

type patchValue[T any] struct{
	Op string `json:"op"`
	Path string `json:"path"`
	Value T `json:"value"`
}

// startMigration starts the VM migration process for a single pod, returning the information it
// needs
//
// This method assumes that the caller currently has a lock on the state.
func (e *AutoscaleEnforcer) startMigration(ctx context.Context, pod *podState) (*api.MigrateResponse, error) {
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

	// Re-mark the VM's 'init-vcpu' label as the current value, so that when the migration is
	// created it'll have the correct initial vCPU:
	if err := e.patchVMInitVCPU(ctx, pod); err != nil {
		return nil, err
	}

	return &api.MigrateResponse{}, nil
}

func (e *AutoscaleEnforcer) patchVMInitVCPU(ctx context.Context, pod *podState) error {
	patchPayload := []patchValue[string]{{
		Op: "replace",
		// if '/' is in the label, it's replaced by '~1'. Source:
		//   https://stackoverflow.com/questions/36147137#comment98654379_36163917
		Path: "/metadata/labels/autoscaler~1init-vcpu",
		Value: fmt.Sprintf("%d", pod.vCPU.reserved), // labels are strings
	}}
	patchPayloadBytes, err := json.Marshal(patchPayload)
	if err != nil {
		err = fmt.Errorf("Error marshalling JSON patch payload: %s", err)
		klog.Errorf("[autoscale-enforcer] %s", err)
		return err
	}

	_, err = e.virtClient.VirtV1alpha1().
		VirtualMachines("default").
		Patch(ctx, pod.vmName, ktypes.JSONPatchType, patchPayloadBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("Error executing VM patch request: %s", err)
	}

	return nil
}
