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
				"[autoscale-enforcer] Responding with status code %d to pod %v: %s",
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
	// Don't migrate if it's disabled
	mustMigrate = mustMigrate && e.state.conf.migrationEnabled()

	permit, status, err := e.handleResources(pod, node, req.Resources, mustMigrate)
	if err != nil {
		return nil, status, err
	}

	var migrateDecision *api.MigrateResponse
	if mustMigrate {
		migrateDecision = &api.MigrateResponse{}
		err = e.state.startMigration(context.Background(), pod, e.vmClient)
		if err != nil {
			return nil, 500, fmt.Errorf("Error starting migration for pod %v: %w", pod.name, err)
		}
	}

	resp := api.PluginResponse{
		Permit:      permit,
		Migrate:     migrateDecision,
		ComputeUnit: *node.computeUnit,
	}
	pod.mostRecentComputeUnit = node.computeUnit // refer to common allocation
	return &resp, 200, nil
}

func (e *AutoscaleEnforcer) handleResources(
	pod *podState, node *nodeState, req api.Resources, startingMigration bool,
) (api.Resources, int, error) {
	// Check that we aren't being asked to do something during migration:
	if pod.currentlyMigrating() {
		// The agent shouldn't have asked for a change after already receiving notice that it's
		// migrating.
		if req.VCPU != pod.vCPU.reserved || req.Mem != pod.memSlots.reserved {
			err := fmt.Errorf("cannot change resources: agent has already been informed that pod is migrating")
			return api.Resources{}, 400, err
		}
		return api.Resources{VCPU: pod.vCPU.reserved, Mem: pod.memSlots.reserved}, 200, nil
	}

	// Check that the resources correspond to an integer number of compute units, based on what the
	// pod was most recently informed of.
	if pod.mostRecentComputeUnit != nil {
		cu := *pod.mostRecentComputeUnit
		dividesCleanly := req.VCPU%cu.VCPU == 0 && req.Mem%cu.Mem == 0 && req.VCPU/cu.VCPU == req.Mem/cu.Mem
		if !dividesCleanly {
			err := fmt.Errorf(
				"requested resources %+v do not divide cleanly by previous compute unit %+v",
				req, cu,
			)
			return api.Resources{}, 400, err
		}
	}

	vCPUTransition := collectResourceTransition(&node.vCPU, &pod.vCPU)
	memTransition := collectResourceTransition(&node.memSlots, &pod.memSlots)

	vCPUVerdict := vCPUTransition.handleRequested(req.VCPU, startingMigration)
	memVerdict := memTransition.handleRequested(req.Mem, startingMigration)

	fmtString := "[autoscale-enforcer] Handled resources from pod %v AgentRequest.\n" +
		"\tvCPU verdict: %s\n" +
		"\t mem verdict: %s"
	klog.Infof(fmtString, pod.name, vCPUVerdict, memVerdict)

	return api.Resources{VCPU: pod.vCPU.reserved, Mem: pod.memSlots.reserved}, 200, nil
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
