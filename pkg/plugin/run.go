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

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

const (
	MaxHTTPBodySize  int64  = 1 << 10 // 1 KiB
	ContentTypeJSON  string = "application/json"
	ContentTypeError string = "text/plain"
)

// The scheduler plugin currently supports v1.0 to v4.0 of the agent<->scheduler plugin protocol.
//
// If you update either of these values, make sure to also update VERSIONING.md.
const (
	MinPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV1_0
	MaxPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV4_0
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

	pod, ok := e.state.pods[req.Pod]
	if !ok {
		logger.Warn("Received request for Pod we don't know") // pod already in the logger's context
		return nil, 404, errors.New("pod not found")
	}
	if pod.vm == nil {
		logger.Error("Received request for non-VM Pod")
		return nil, 400, errors.New("pod is not associated with a VM")
	}

	// If the request was actually sending a quantity of *memory slots*, rather than bytes, then
	// multiply memory resources to make it match the
	if !req.ProtoVersion.RepresentsMemoryAsBytes() {
		req.Resources.Mem *= pod.vm.memSlotSize
	}

	node := pod.node
	nodeName = node.name // set nodeName for deferred metrics

	// Also, now that we know which VM this refers to (and which node it's on), add that to the logger for later.
	logger = logger.With(zap.Object("virtualmachine", pod.vm.name), zap.String("node", nodeName))

	mustMigrate := pod.vm.migrationState == nil &&
		// Check whether the pod *will* migrate, then update its resources, and THEN start its
		// migration, using the possibly-changed resources.
		e.updateMetricsAndCheckMustMigrate(logger, pod.vm, node, req.Metrics) &&
		// Don't migrate if it's disabled
		e.state.conf.migrationEnabled()

	supportsFractionalCPU := req.ProtoVersion.SupportsFractionalCPU()

	permit, status, err := e.handleResources(logger, pod, node, req.Resources, req.LastPermit, mustMigrate, supportsFractionalCPU)
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
		ComputeUnit: getComputeUnitForResponse(e.state.conf.ComputeUnit, req.ProtoVersion),
	}

	// If the selected protocol version is using memory slots, rather than byte quantities, then we
	// should convert the values before responding.
	if !req.ProtoVersion.RepresentsMemoryAsBytes() {
		resp.Permit.Mem /= pod.vm.memSlotSize
		if resp.ComputeUnit != nil {
			resp.ComputeUnit.Mem /= pod.vm.memSlotSize
		}
	}

	pod.vm.mostRecentComputeUnit = &e.state.conf.ComputeUnit
	return &resp, 200, nil
}

// getComputeUnitForResponse tries to return compute unit that the agent supports
//
// If the plugin is not supposed to send a compute unit in this version of the protocol, then we
// return nil.
// Else if our compute unit has a fractional CPU but the agent doesn't support that, we multiply the
// result until it's no longer fractional.
func getComputeUnitForResponse(computeUnit api.Resources, protoVersion api.PluginProtoVersion) *api.Resources {
	if !protoVersion.PluginSendsComputeUnit() {
		return nil
	}

	if !protoVersion.SupportsFractionalCPU() {
		initialCU := computeUnit
		for i := uint16(2); computeUnit.VCPU%1000 != 0; i++ {
			computeUnit = initialCU.Mul(i)
		}
	}

	return &computeUnit
}

func (e *AutoscaleEnforcer) handleResources(
	logger *zap.Logger,
	pod *podState,
	node *nodeState,
	req api.Resources,
	lastPermit *api.Resources,
	startingMigration bool,
	supportsFractionalCPU bool,
) (api.Resources, int, error) {
	if !supportsFractionalCPU && req.VCPU%1000 != 0 {
		err := errors.New("agent requested fractional CPU with protocol version that does not support it")
		return api.Resources{}, 400, err
	}

	// Check that we aren't being asked to do something during migration:
	if pod.vm.currentlyMigrating() {
		// The agent shouldn't have asked for a change after already receiving notice that it's
		// migrating.
		if req.VCPU != pod.cpu.Reserved || req.Mem != pod.mem.Reserved {
			err := errors.New("cannot change resources: agent has already been informed that pod is migrating")
			return api.Resources{}, 400, err
		}
		return api.Resources{VCPU: pod.cpu.Reserved, Mem: pod.mem.Reserved}, 200, nil
	}

	// Check that the resources correspond to an integer number of compute units, based on what the
	// pod was most recently informed of. The resources may only be mismatched if one of them is at
	// the minimum or maximum of what's allowed for this VM.
	if pod.vm.mostRecentComputeUnit != nil {
		cu := *pod.vm.mostRecentComputeUnit
		dividesCleanly := req.VCPU%cu.VCPU == 0 && req.Mem%cu.Mem == 0 && uint32(req.VCPU/cu.VCPU) == uint32(req.Mem/cu.Mem)
		atMin := req.VCPU == pod.cpu.Min || req.Mem == pod.mem.Min
		atMax := req.VCPU == pod.cpu.Max || req.Mem == pod.mem.Max
		if !dividesCleanly && !(atMin || atMax) {
			contextString := "If the VM's bounds did not just change, then this indicates a bug in the autoscaler-agent."
			logger.Warn(
				"Pod requested resources do not divide cleanly by previous compute unit",
				zap.Object("requested", req), zap.Object("computeUnit", cu), zap.String("context", contextString),
			)
		}
	}

	if lastPermit != nil {
		cpuVerdict := makeResourceTransitioner(&node.cpu, &pod.cpu).
			handleLastPermit(lastPermit.VCPU)
		memVerdict := makeResourceTransitioner(&node.mem, &pod.mem).
			handleLastPermit(lastPermit.Mem)
		logger.Info(
			"Handled last permit info from pod",
			zap.Object("verdict", verdictSet{
				cpu: cpuVerdict,
				mem: memVerdict,
			}),
		)
	}

	cpuFactor := vmapi.MilliCPU(1)
	if !supportsFractionalCPU {
		cpuFactor = 1000
	}
	memFactor := pod.vm.memSlotSize

	cpuVerdict := makeResourceTransitioner(&node.cpu, &pod.cpu).
		handleRequested(req.VCPU, startingMigration, cpuFactor)
	memVerdict := makeResourceTransitioner(&node.mem, &pod.mem).
		handleRequested(req.Mem, startingMigration, memFactor)

	logger.Info(
		"Handled requested resources from pod",
		zap.Object("verdict", verdictSet{
			cpu: cpuVerdict,
			mem: memVerdict,
		}),
	)

	return api.Resources{VCPU: pod.cpu.Reserved, Mem: pod.mem.Reserved}, 200, nil
}

func (e *AutoscaleEnforcer) updateMetricsAndCheckMustMigrate(
	logger *zap.Logger,
	vm *vmPodState,
	node *nodeState,
	metrics *api.Metrics,
) bool {
	// This pod should migrate if (a) we're looking for migrations and (b) it's next up in the
	// priority queue. We will give it a chance later to veto if the metrics have changed too much
	//
	// A third condition, "the pod is marked to always migrate" causes it to migrate even if neither
	// of the above conditions are met, so long as it has *previously* provided metrics.
	shouldMigrate := node.mq.isNextInQueue(vm) && node.tooMuchPressure(logger)
	forcedMigrate := vm.testingOnlyAlwaysMigrate && vm.metrics != nil

	logger.Info("Updating pod metrics", zap.Any("metrics", metrics))
	oldMetrics := vm.metrics
	vm.metrics = metrics
	if vm.currentlyMigrating() {
		return false // don't do anything else; it's already migrating.
	}

	node.mq.addOrUpdate(vm)

	if !shouldMigrate && !forcedMigrate {
		return false
	}

	// Give the pod a chance to veto migration if its metrics have significantly changed...
	var veto error
	if oldMetrics != nil && !forcedMigrate {
		veto = vm.checkOkToMigrate(*oldMetrics)
	}

	// ... but override the veto if it's still the best candidate anyways.
	stillFirst := node.mq.isNextInQueue(vm)

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
