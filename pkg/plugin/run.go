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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/patch"
)

const (
	MaxHTTPBodySize  int64  = 1 << 10 // 1 KiB
	ContentTypeJSON  string = "application/json"
	ContentTypeError string = "text/plain"
)

const (
	MinPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV5_0
	MaxPluginProtocolVersion api.PluginProtoVersion = api.PluginProtoV5_0
)

// startPermitHandler runs the server for handling each resourceRequest from a pod
func (s *PluginState) startPermitHandler(
	ctx context.Context,
	logger *zap.Logger,
	getPod func(util.NamespacedName) (*corev1.Pod, bool),
	listenerForPod func(types.UID) (util.BroadcastReceiver, bool),
) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger := logger // copy locally, so that we can add fields and refer to it in defers

		var finalStatus int

		defer func() {
			s.metrics.ResourceRequests.WithLabelValues(strconv.Itoa(finalStatus)).Inc()
		}()

		// Catch any potential panics and report them as 500s
		defer func() {
			if err := recover(); err != nil {
				msg := "request handler panicked"
				logger.Error(msg, zap.String("error", fmt.Sprint(err)))
				finalStatus = 500
				w.WriteHeader(finalStatus)
				_, _ = w.Write([]byte(msg))
			}
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

		logger = logger.With(zap.Object("pod", req.Pod), zap.Any("request", req))

		resp, statusCode, err := s.handleAgentRequest(logger, req, getPod, listenerForPod)
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
func (s *PluginState) handleAgentRequest(
	logger *zap.Logger,
	req api.AgentRequest,
	getPod func(util.NamespacedName) (*corev1.Pod, bool),
	listenerForPod func(types.UID) (util.BroadcastReceiver, bool),
) (_ *api.PluginResponse, status int, _ error) {
	nodeName := "<none>" // override this later if we have a node name

	defer func() {
		s.metrics.ValidResourceRequests.
			WithLabelValues(strconv.Itoa(status), nodeName).
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

	// check that req.ComputeUnit has no zeros
	if err := req.ComputeUnit.ValidateNonZero(); err != nil {
		return nil, 400, fmt.Errorf("computeUnit fields must be non-zero: %w", err)
	}

	podObj, ok := getPod(req.Pod)
	if !ok {
		logger.Warn("Received request for Pod we don't know") // pod already in the logger's context
		return nil, 404, errors.New("pod not found")
	} else if podObj.Spec.NodeName == "" {
		logger.Warn("Received request for Pod we don't know where it was scheduled")
		return nil, 404, errors.New("pod's node is unknown")
	}

	nodeName = podObj.Spec.NodeName // set nodeName for deferred metrics

	vmRef, ok := vmv1.VirtualMachineOwnerForPod(podObj)
	if !ok {
		logger.Error("Received request for non-VM Pod")
		return nil, 400, errors.New("pod is not associated with a VM")
	}
	vmName := util.NamespacedName{
		Namespace: podObj.Namespace,
		Name:      vmRef.Name,
	}

	// From this point, we'll:
	//
	// 1. Update the annotations on the VirtualMachine object, if this request should change them;
	//    and
	//
	// 2. Wait for the annotations on the Pod object to change so that the approved resources are
	//    increased towards what was requested -- only if the amount requested was greater than what
	//    was last approved.

	patches, changed := vmPatchForAgentRequest(podObj, req)

	// Start listening *before* we update the VM.
	updateReceiver, podExists := listenerForPod(podObj.UID)

	// Only patch the VM object if it changed:
	if changed {
		if err := s.patchVM(vmName, patches); err != nil {
			logger.Error("Failed to patch VM object", zap.Error(err))
			return nil, 500, errors.New("failed to patch VM object")
		}
		logger.Info("Patched VirtualMachine for agent request", zap.Any("patches", patches))
	}

	// If we should be able to instantly approve the request, don't bother waiting to observe it.
	if req.LastPermit != nil && !req.Resources.HasFieldGreaterThan(*req.LastPermit) {
		resp := api.PluginResponse{
			Permit:  req.Resources,
			Migrate: nil,
		}
		status = 200
		logger.Info("Handled agent request", zap.Int("status", status), zap.Any("response", resp))
		return &resp, status, nil
	}

	// We want to wait for updates on the pod, but if it no longer exists, we should just return.
	if !podExists {
		logger.Warn("Pod for request no longer exists")
		return nil, 404, errors.New("pod not found")
	}

	// FIXME: make the timeout configurable.
	updateTimeout := time.NewTimer(time.Second)
	defer updateTimeout.Stop()

	for {
		timedOut := false

		// Only listen for updates if we need to wait.
		needToWait := req.LastPermit == nil || req.LastPermit.HasFieldLessThan(req.Resources)
		if needToWait {
			select {
			case <-updateTimeout.C:
				timedOut = true
			case <-updateReceiver.Wait():
				updateReceiver.Awake()
			}
		}

		podObj, ok := getPod(req.Pod)
		if !ok {
			logger.Warn("Pod for request on longer exists")
			return nil, 404, errors.New("pod not found")
		}

		podState, err := state.PodStateFromK8sObj(podObj)
		if err != nil {
			logger.Error("Failed to extract Pod state from Pod object for agent request")
			return nil, 500, errors.New("failed to extract state from pod")
		}

		// Reminder: We're only listening for updates if the requested resources are greater than
		// what was last approved.
		//
		// So, we should keep waiting until the approved resources have increased from the
		// LastPermit in the request.

		approved := api.Resources{
			VCPU: podState.CPU.Reserved,
			Mem:  podState.Mem.Reserved,
		}
		requested := api.Resources{
			VCPU: podState.CPU.Requested,
			Mem:  podState.Mem.Requested,
		}

		canReturn := requested == req.Resources
		var shouldReturn bool
		if req.LastPermit == nil {
			_, hasApproved := podObj.Annotations[api.InternalAnnotationResourcesApproved]
			shouldReturn = canReturn && hasApproved
		} else {
			shouldReturn = canReturn && approved.HasFieldGreaterThan(*req.LastPermit)
		}

		// Return if we have results, or if we've timed out and it's good enough.
		if shouldReturn || (timedOut && canReturn) {
			if timedOut {
				logger.Warn("Timed out while waiting for updates to respond to agent request")
			}
			resp := api.PluginResponse{
				Permit:  approved,
				Migrate: nil,
			}
			status = 200
			logger.Info("Handled agent request", zap.Int("status", status), zap.Any("response", resp))
			return &resp, status, nil
		}

		// ... otherwise, if we timed out and our updates to the VM *haven't* yet been reflected on
		// the pod, we don't have anything we can return, so we should return an error.
		if timedOut {
			logger.Error("Timed out while waiting for updates without suitable response to agent request")
			return nil, 500, errors.New("timed out waiting for updates to be processed")
		}

		// ... other-otherwise, we'll wait for more updates.
		continue
	}
}

func vmPatchForAgentRequest(pod *corev1.Pod, req api.AgentRequest) (_ []patch.Operation, changed bool) {
	marshalJSON := func(value any) string {
		bs, err := json.Marshal(value)
		if err != nil {
			panic(fmt.Sprintf("failed to marshal value: %s", err))
		}
		return string(bs)
	}

	var patches []patch.Operation

	computeUnitJSON := marshalJSON(req.ComputeUnit)
	if computeUnitJSON != pod.Annotations[api.AnnotationAutoscalingUnit] {
		changed = true
	}
	// Always include the patch, even if it's the same as current. We'll only execute it if
	// there's differences from what's there currently.
	patches = append(patches, patch.Operation{
		Op: patch.OpReplace,
		Path: fmt.Sprintf(
			"/metadata/annotations/%s",
			patch.PathEscape(api.AnnotationAutoscalingUnit),
		),
		Value: computeUnitJSON,
	})

	requestedJSON := marshalJSON(req.Resources)
	if requestedJSON != pod.Annotations[api.InternalAnnotationResourcesRequested] {
		changed = true
	}
	patches = append(patches, patch.Operation{
		Op: patch.OpReplace,
		Path: fmt.Sprintf(
			"/metadata/annotations/%s",
			patch.PathEscape(api.InternalAnnotationResourcesRequested),
		),
		Value: requestedJSON,
	})

	if req.LastPermit != nil {
		approvedJSON := marshalJSON(*req.LastPermit)
		if approvedJSON != pod.Annotations[api.InternalAnnotationResourcesApproved] {
			changed = true
		}
		patches = append(patches, patch.Operation{
			Op: patch.OpReplace,
			Path: fmt.Sprintf(
				"/metadata/annotations/%s",
				patch.PathEscape(api.InternalAnnotationResourcesApproved),
			),
			Value: approvedJSON,
		})
	}

	return patches, changed
}
