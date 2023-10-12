package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/api"
)

const (
	MaxHTTPBodySize  int64  = 1 << 10 // 1 KiB
	ContentTypeJSON  string = "application/json"
	ContentTypeError string = "text/plain"
)

// The scheduler plugin currently supports v1.0 to v1.1 of the agent<->scheduler plugin protocol.
//
// If you update either of these values, make sure to also update VERSIONING.md.
const (
	MinPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV1_0
	MaxPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV2_0
)

// startPermitHandler runs the server for handling each resourceRequest from a pod
func (e *AutoscaleEnforcer) startPermitHandler(ctx context.Context, logger *zap.Logger) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var finalStatus int
		defer func() {
			e.metrics.resourceRequests.WithLabelValues(r.RemoteAddr, strconv.Itoa(finalStatus)).Inc()
		}()

		if r.Method != "POST" {
			finalStatus = 400
			w.WriteHeader(400)
			_, _ = w.Write([]byte("must be POST"))
			return
		}

		defer r.Body.Close()
		var req api.AgentRequest
		jsonDecoder := json.NewDecoder(io.LimitReader(r.Body, MaxHTTPBodySize))
		if err := jsonDecoder.Decode(&req); err != nil {
			logger.Warn("Received bad JSON in request", zap.Error(err))
			w.Header().Add("Content-Type", ContentTypeError)
			finalStatus = 400
			w.WriteHeader(400)
			_, _ = w.Write([]byte("bad JSON"))
			return
		}

		logger := logger.With(zap.Object("pod", req.Pod))
		logger.Info(
			"Received autoscaler-agent request",
			zap.String("client", r.RemoteAddr), zap.Any("request", req),
		)

		resp, statusCode, err := e.handleAgentRequest(logger, req)
		finalStatus = statusCode

		if err != nil {
			logFunc := logger.Warn
			if 500 <= statusCode && statusCode < 600 {
				logFunc = logger.Error
			}

			logFunc(
				"Responding to autoscaler-agent request with error",
				zap.Int("status", statusCode),
				zap.Error(err),
			)

			w.Header().Add("Content-Type", ContentTypeError)
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(err.Error()))
			return
		}

		responseBody, err := json.Marshal(&resp)
		if err != nil {
			logger.Panic("Failed to encode response JSON", zap.Error(err))
		}

		logger.Info(
			"Responding to autoscaler-agent request",
			zap.Int("status", statusCode),
			zap.Any("response", resp),
		)

		w.Header().Add("Content-Type", ContentTypeJSON)
		w.WriteHeader(statusCode)
		_, _ = w.Write(responseBody)
	})

	orca := srv.GetOrchestrator(ctx)

	logger.Info("Starting resource request server")
	hs := srv.HTTP("resource-request", 5*time.Second, &http.Server{Addr: "0.0.0.0:10299", Handler: mux})
	if err := hs.Start(ctx); err != nil {
		return fmt.Errorf("Error starting resource request server: %w", err)
	}

	if err := orca.Add(hs); err != nil {
		return fmt.Errorf("Error adding resource request server to orchestrator: %w", err)
	}
	return nil
}

// Returns body (if successful), status code, error (if unsuccessful)
func (e *AutoscaleEnforcer) handleAgentRequest(
	logger *zap.Logger,
	req api.AgentRequest,
) (_ *api.PluginResponse, status int, _ error) {
	nodeName := "<none>" // override this later if we have a node name
	defer func() {
		hasMetrics := req.Metrics != nil
		e.metrics.validResourceRequests.
			WithLabelValues(strconv.Itoa(status), nodeName, strconv.FormatBool(hasMetrics)).
			Inc()
	}()

	// Before doing anything, check that the version is within the range we're expecting.
	expectedProtoRange := api.VersionRange[api.PluginProtoVersion]{
		Min: MinPluginProtocolVersion,
		Max: MaxPluginProtocolVersion,
	}

	if !req.ProtoVersion.IsValid() {
		return nil, 400, fmt.Errorf("Invalid protocol version %v", req.ProtoVersion)
	}
	reqProtoRange := req.ProtocolRange()
	if _, ok := expectedProtoRange.LatestSharedVersion(reqProtoRange); !ok {
		return nil, 400, fmt.Errorf(
			"Protocol version mismatch: Need %v but got %v", expectedProtoRange, reqProtoRange,
		)
	}

	// if req.Metrics is nil, check that the protocol version allows that.
	if req.Metrics == nil && !req.ProtoVersion.AllowsNilMetrics() {
		return nil, 400, fmt.Errorf("nil metrics not supported for protocol version %v", req.ProtoVersion)
	}

	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	pod, ok := e.state.podMap[req.Pod]
	if !ok {
		logger.Warn("Received request for pod we don't know") // pod already in the logger's context
		return nil, 404, errors.New("pod not found")
	}

	node := pod.node
	nodeName = node.name // set nodeName for deferred metrics

	// Also, now that we know which VM this refers to (and which node it's on), add that to the logger for later.
	logger = logger.With(zap.Object("virtualmachine", pod.vmName), zap.String("node", nodeName))

	mustMigrate := pod.migrationState == nil &&
		// Check whether the pod *will* migrate, then update its resources, and THEN start its
		// migration, using the possibly-changed resources.
		e.updateMetricsAndCheckMustMigrate(logger, pod, node, req.Metrics) &&
		// Don't migrate if it's disabled
		e.state.conf.migrationEnabled()

	supportsFractionalCPU := req.ProtoVersion.SupportsFractionalCPU()

	permit, status, err := e.handleResources(logger, pod, node, req.Resources, mustMigrate, supportsFractionalCPU)
	if err != nil {
		return nil, status, err
	}

	var migrateDecision *api.MigrateResponse
	if mustMigrate {
		created, err := e.startMigration(context.Background(), logger, pod)
		if err != nil {
			return nil, 500, fmt.Errorf("Error starting migration for pod %v: %w", pod.name, err)
		}

		// We should only signal to the autoscaler-agent that we've started migrating if we actually
		// *created* the migration. We're not *supposed* to receive requests for a VM that's already
		// migrating, so receiving one means that *something*'s gone wrong. If that's on us, we
		// should try to avoid
		if created {
			migrateDecision = &api.MigrateResponse{}
		}
	}

	resp := api.PluginResponse{
		Permit:      permit,
		Migrate:     migrateDecision,
		ComputeUnit: getComputeUnitForResponse(node, supportsFractionalCPU),
	}
	pod.mostRecentComputeUnit = node.computeUnit // refer to common allocation
	return &resp, 200, nil
}

// getComputeUnitForResponse tries to return compute unit that the agent supports
// if we have a fractional CPU but the agent does not support it we multiply the result
func getComputeUnitForResponse(node *nodeState, supportsFractional bool) api.Resources {
	computeUnit := *node.computeUnit
	if !supportsFractional {
		initialCU := computeUnit
		for i := uint16(2); computeUnit.VCPU%1000 != 0; i++ {
			computeUnit = initialCU.Mul(i)
		}
	}

	return computeUnit
}

func (e *AutoscaleEnforcer) handleResources(
	logger *zap.Logger,
	pod *podState,
	node *nodeState,
	req api.Resources,
	startingMigration bool,
	supportsFractionalCPU bool,
) (api.Resources, int, error) {
	if !supportsFractionalCPU && req.VCPU%1000 != 0 {
		err := errors.New("agent requested fractional CPU with protocol version that does not support it")
		return api.Resources{}, 400, err
	}

	// Check that we aren't being asked to do something during migration:
	if pod.currentlyMigrating() {
		// The agent shouldn't have asked for a change after already receiving notice that it's
		// migrating.
		if req.VCPU != pod.vCPU.Reserved || req.Mem != pod.memSlots.Reserved {
			err := errors.New("cannot change resources: agent has already been informed that pod is migrating")
			return api.Resources{}, 400, err
		}
		return api.Resources{VCPU: pod.vCPU.Reserved, Mem: pod.memSlots.Reserved}, 200, nil
	}

	// Check that the resources correspond to an integer number of compute units, based on what the
	// pod was most recently informed of. The resources may only be mismatched if one of them is at
	// the minimum or maximum of what's allowed for this VM.
	if pod.mostRecentComputeUnit != nil {
		cu := *pod.mostRecentComputeUnit
		dividesCleanly := req.VCPU%cu.VCPU == 0 && req.Mem%cu.Mem == 0 && uint32(req.VCPU/cu.VCPU) == uint32(req.Mem/cu.Mem)
		atMin := req.VCPU == pod.vCPU.Min || req.Mem == pod.memSlots.Min
		atMax := req.VCPU == pod.vCPU.Max || req.Mem == pod.memSlots.Max
		if !dividesCleanly && !(atMin || atMax) {
			contextString := "If the VM's bounds did not just change, then this indicates a bug in the autoscaler-agent."
			logger.Warn(
				"Pod requested resources do not divide cleanly by previous compute unit",
				zap.Object("requested", req), zap.Object("computeUnit", cu), zap.String("context", contextString),
			)
		}
	}

	vCPUTransition := collectResourceTransition(&node.vCPU, &pod.vCPU)
	memTransition := collectResourceTransition(&node.memSlots, &pod.memSlots)

	vCPUVerdict := vCPUTransition.handleRequested(req.VCPU, startingMigration, !supportsFractionalCPU)
	memVerdict := memTransition.handleRequested(req.Mem, startingMigration, false)

	logger.Info(
		"Handled resources from pod",
		zap.Object("verdict", verdictSet{
			cpu: vCPUVerdict,
			mem: memVerdict,
		}),
	)

	return api.Resources{VCPU: pod.vCPU.Reserved, Mem: pod.memSlots.Reserved}, 200, nil
}

func (e *AutoscaleEnforcer) updateMetricsAndCheckMustMigrate(
	logger *zap.Logger,
	pod *podState,
	node *nodeState,
	metrics *api.Metrics,
) bool {
	// This pod should migrate if (a) we're looking for migrations and (b) it's next up in the
	// priority queue. We will give it a chance later to veto if the metrics have changed too much
	//
	// A third condition, "the pod is marked to always migrate" causes it to migrate even if neither
	// of the above conditions are met, so long as it has *previously* provided metrics.
	shouldMigrate := node.mq.isNextInQueue(pod) && node.tooMuchPressure(logger)
	forcedMigrate := pod.testingOnlyAlwaysMigrate && pod.metrics != nil

	logger.Info("Updating pod metrics", zap.Any("metrics", metrics))
	oldMetrics := pod.metrics
	pod.metrics = metrics
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
			logger.Info("Pod attempted veto of self migration, still highest priority", zap.NamedError("veto", veto))
		}

		return true
	} else {
		logger.Warn("Pod vetoed self migration", zap.NamedError("veto", veto))
		return false
	}
}
