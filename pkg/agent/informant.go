package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tychoish/fun/srv"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// The autoscaler-agent currently supports v1.0 to v2.0 of the agent<->informant protocol.
//
// If you update either of these values, make sure to also update VERSIONING.md.
const (
	MinInformantProtocolVersion api.InformantProtoVersion = api.InformantProtoV1_0
	MaxInformantProtocolVersion api.InformantProtoVersion = api.InformantProtoV2_0
)

type InformantServer struct {
	// runner is the Runner currently responsible for this InformantServer. We must acquire its lock
	// before making any updates to other fields of this struct
	runner *Runner

	// desc is the AgentDesc describing this VM informant server. This field is immutable.
	desc api.AgentDesc

	seqNum uint64
	// receivedIDCheck is true if the server has received at least one successful request at the /id
	// endpoint by the expected IP address of the VM
	//
	// This field is used to check for protocol violations (i.e. responding to /register without
	// checking with /id), and *may* help prevent certain IP-spoofing based attacks - although the
	// security implications are entirely speculation.
	receivedIDCheck bool

	// madeContact is true if any request to the VM informant could have interacted with it.
	//
	// If madeContact is false, then mode is guaranteed to be InformantServerUnconfirmed, so
	// madeContact only needs to be set on /register requests (because all others require a
	// successful register first).
	//
	// This field MUST NOT be updated without holding BOTH runner.lock and requestLock.
	//
	// This field MAY be read while holding EITHER runner.lock OR requestLock.
	madeContact bool

	// protoVersion gives the version of the agent<->informant protocol currently in use, if the
	// server has been confirmed.
	//
	// In other words, this field is not nil if and only if mode is not InformantServerUnconfirmed.
	protoVersion *api.InformantProtoVersion

	// mode indicates whether the informant has marked the connection as resumed or not
	//
	// This field MUST NOT be updated without holding BOTH runner.lock AND requestLock.
	//
	// This field MAY be read while holding EITHER runner.lock OR requestLock.
	mode InformantServerMode

	// updatedInformant is signalled once, when the InformantServer's register request completes,
	// and the value of runner.informant is updated.
	updatedInformant util.CondChannelSender

	// upscaleRequested is signalled whenever a valid request on /try-upscale is received, with at
	// least one field set to true (i.e., at least one resource is being requested).
	upscaleRequested util.CondChannelSender

	// requestLock guards requests to the VM informant to make sure that only one request is being
	// made at a time.
	//
	// If both requestLock and runner.lock are required, then requestLock MUST be acquired before
	// runner.lock.
	requestLock util.ChanMutex

	// exitStatus holds some information about why the server exited
	exitStatus *InformantServerExitStatus

	// exit signals that the server should shut down, and sets exitStatus to status.
	//
	// This function MUST be called while holding runner.lock.
	exit func(status InformantServerExitStatus)
}

type InformantServerMode string

const (
	InformantServerUnconfirmed InformantServerMode = "unconfirmed"
	InformantServerSuspended   InformantServerMode = "suspended"
	InformantServerRunning     InformantServerMode = "running"
)

// InformantServerState is the serializable state of the InformantServer, produced by calls to the
// Runner's State() method.
type InformantServerState struct {
	Desc            api.AgentDesc              `json:"desc"`
	SeqNum          uint64                     `json:"seqNum"`
	ReceivedIDCheck bool                       `json:"receivedIDCheck"`
	MadeContact     bool                       `json:"madeContact"`
	ProtoVersion    *api.InformantProtoVersion `json:"protoVersion"`
	Mode            InformantServerMode        `json:"mode"`
	ExitStatus      *InformantServerExitStatus `json:"exitStatus"`
}

type InformantServerExitStatus struct {
	// Err is the error, if any, that caused the server to exit. This is only non-nil when context
	// used to start the server becomes canceled (i.e. the Runner is exiting).
	Err error
	// RetryShouldFix is true if simply retrying should resolve err. This is true when e.g. the
	// informant responds with a 404 to a downscale or upscale request - it might've restarted, so
	// we just need to re-register.
	RetryShouldFix bool
}

// NewInformantServer starts an InformantServer, returning it and a signal receiver that will be
// signalled when it exits.
func NewInformantServer(
	ctx context.Context,
	logger *zap.Logger,
	runner *Runner,
	updatedInformant util.CondChannelSender,
	upscaleRequested util.CondChannelSender,
) (*InformantServer, util.SignalReceiver[struct{}], error) {
	// Manually start the TCP listener so that we can see the port it's assigned
	addr := net.TCPAddr{IP: net.IPv4zero, Port: 0 /* 0 means it'll be assigned any(-ish) port */}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return nil, util.SignalReceiver[struct{}]{}, fmt.Errorf("Error listening on TCP: %w", err)
	}

	// Get back the assigned port
	var serverAddr string
	switch addr := listener.Addr().(type) {
	case *net.TCPAddr:
		serverAddr = fmt.Sprintf("%s:%d", runner.global.podIP, addr.Port)
	default:
		panic(errors.New("unexpected net.Addr type"))
	}

	server := &InformantServer{
		runner: runner,
		desc: api.AgentDesc{
			AgentID:         uuid.New(),
			ServerAddr:      serverAddr,
			MinProtoVersion: MinInformantProtocolVersion,
			MaxProtoVersion: MaxInformantProtocolVersion,
		},
		seqNum:           0,
		receivedIDCheck:  false,
		madeContact:      false,
		protoVersion:     nil,
		mode:             InformantServerUnconfirmed,
		updatedInformant: updatedInformant,
		upscaleRequested: upscaleRequested,
		requestLock:      util.NewChanMutex(),
		exitStatus:       nil,
		exit:             nil, // see below.
	}

	logger = logger.With(zap.Object("server", server.desc))
	logger.Info("Starting Informant server")

	mux := http.NewServeMux()
	util.AddHandler(logger, mux, "/id", http.MethodGet, "struct{}", server.handleID)
	util.AddHandler(logger, mux, "/resume", http.MethodPost, "ResumeAgent", server.handleResume)
	util.AddHandler(logger, mux, "/suspend", http.MethodPost, "SuspendAgent", server.handleSuspend)
	util.AddHandler(logger, mux, "/try-upscale", http.MethodPost, "MoreResourcesRequest", server.handleTryUpscale)
	httpServer := &http.Server{Handler: mux}

	sendFinished, recvFinished := util.NewSingleSignalPair[struct{}]()
	backgroundCtx, cancelBackground := context.WithCancel(ctx)

	// note: docs for server.exit guarantee this function is called while holding runner.lock.
	server.exit = func(status InformantServerExitStatus) {
		sendFinished.Send(struct{}{})
		cancelBackground()

		// Set server.exitStatus if isn't already
		if server.exitStatus == nil {
			server.exitStatus = &status
			logFunc := logger.Warn
			if status.RetryShouldFix {
				logFunc = logger.Info
			}

			logFunc("Informant server exiting", zap.Bool("retry", status.RetryShouldFix), zap.Error(status.Err))
		}

		// we need to spawn these in separate threads so the caller doesn't block while holding
		// runner.lock
		runner.spawnBackgroundWorker(srv.GetBaseContext(ctx), logger, "InformantServer shutdown", func(_ context.Context, logger *zap.Logger) {
			// we want shutdown to (potentially) live longer than the request which
			// made it, but having a timeout is still good.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := httpServer.Shutdown(ctx); err != nil {
				logger.Warn("Error shutting down InformantServer", zap.Error(err))
			}
		})
		if server.madeContact {
			// only unregister the server if we could have plausibly contacted the informant
			runner.spawnBackgroundWorker(srv.GetBaseContext(ctx), logger, "InformantServer unregister", func(_ context.Context, logger *zap.Logger) {
				// we want shutdown to (potentially) live longer than the request which
				// made it, but having a timeout is still good.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				if err := server.unregisterFromInformant(ctx, logger); err != nil {
					logger.Warn("Error unregistering", zap.Error(err))
				}
			})
		}
	}

	// Deadlock checker for server.requestLock
	//
	// FIXME: make these timeouts/delays separately defined constants, or configurable
	deadlockChecker := server.requestLock.DeadlockChecker(5*time.Second, time.Second)
	runner.spawnBackgroundWorker(backgroundCtx, logger, "InformantServer deadlock checker", ignoreLogger(deadlockChecker))

	// Main thread running the server. After httpServer.Serve() completes, we do some error
	// handling, but that's about it.
	runner.spawnBackgroundWorker(ctx, logger, "InformantServer", func(c context.Context, logger *zap.Logger) {
		if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("InformantServer exited with unexpected error", zap.Error(err))
		}

		// set server.exitStatus if it isn't already -- generally this should only occur if err
		// isn't http.ErrServerClosed, because other server exits should be controlled by
		runner.lock.Lock()
		defer runner.lock.Unlock()

		if server.exitStatus == nil {
			server.exitStatus = &InformantServerExitStatus{
				Err:            fmt.Errorf("Unexpected exit: %w", err),
				RetryShouldFix: false,
			}
		}
	})

	// Thread waiting for the context to be canceled so we can use it to shut down the server
	runner.spawnBackgroundWorker(ctx, logger, "InformantServer shutdown waiter", func(context.Context, *zap.Logger) {
		// Wait until parent context OR server's context is done.
		<-backgroundCtx.Done()
		server.exit(InformantServerExitStatus{Err: nil, RetryShouldFix: false})
	})

	runner.spawnBackgroundWorker(backgroundCtx, logger, "InformantServer health-checker", func(c context.Context, logger *zap.Logger) {
		// FIXME: make this duration configurable
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.Done():
				return
			case <-ticker.C:
			}

			var done bool
			func() {
				server.requestLock.Lock()
				defer server.requestLock.Unlock()

				// If we've already registered with the informant, and it doesn't support health
				// checks, exit.
				if server.protoVersion != nil && !server.protoVersion.AllowsHealthCheck() {
					logger.Info("Aborting future informant health checks because it does not support them")
					done = true
					return
				}

				if _, err := server.HealthCheck(c, logger); err != nil {
					logger.Warn("Informant health check failed", zap.Error(err))
				}
			}()
			if done {
				return
			}
		}
	})

	return server, recvFinished, nil
}

var (
	InformantServerAlreadyExitedError error = errors.New("Informant server has already exited")
	InformantServerSuspendedError     error = errors.New("Informant server is currently suspended")
	InformantServerUnconfirmedError   error = errors.New("Informant server has not yet been confirmed")
	InformantServerNotCurrentError    error = errors.New("Informant server has been replaced")
)

// IsNormalInformantError returns true if the error is one of the "expected" errors that can occur
// in valid exchanges - due to unavoidable raciness or otherwise.
func IsNormalInformantError(err error) bool {
	return errors.Is(err, InformantServerAlreadyExitedError) ||
		errors.Is(err, InformantServerSuspendedError) ||
		errors.Is(err, InformantServerUnconfirmedError) ||
		errors.Is(err, InformantServerNotCurrentError)
}

// valid checks if the InformantServer is good to use for communication, returning an error if not
//
// This method can return errors for a number of unavoidably-racy protocol states - errors from this
// method should be handled as unusual, but not unexpected. Any error returned will be one of
// InformantServer{AlreadyExited,Suspended,Confirmed}Error.
//
// This method MUST be called while holding s.runner.lock.
func (s *InformantServer) valid() error {
	if s.exitStatus != nil {
		return InformantServerAlreadyExitedError
	}

	switch s.mode {
	case InformantServerRunning:
		// all good; one more check
	case InformantServerUnconfirmed:
		return InformantServerUnconfirmedError
	case InformantServerSuspended:
		return InformantServerSuspendedError
	default:
		panic(fmt.Errorf("Unexpected InformantServerMode %q", s.mode))
	}

	if s.runner.server != s {
		return InformantServerNotCurrentError
	}
	return nil
}

// ExitStatus returns the InformantServerExitStatus associated with the server, if it has been
// instructed to exit
//
// This method MUST NOT be called while holding s.runner.lock.
func (s *InformantServer) ExitStatus() *InformantServerExitStatus {
	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	return s.exitStatus
}

// setLastInformantError is a helper method to abbreviate setting the Runner's lastInformantError
// field. If runnerLocked is true, s.runner.lock will be acquired.
//
// This method MUST be called while holding s.requestLock AND EITHER holding s.runner.lock OR
// runnerLocked MUST be true.
func (s *InformantServer) setLastInformantError(err error, runnerLocked bool) {
	if !runnerLocked {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()
	}

	if s.runner.server == s {
		s.runner.lastInformantError = err
	}
}

// RegisterWithInformant sends a /register request to the VM Informant
//
// If called after a prior success, this method will panic. If the server has already exited, this
// method will return InformantServerAlreadyExitedError.
//
// On certain errors, this method will force the server to exit. This can be checked by calling
// s.ExitStatus() and checking for a non-nil result.
//
// This method MUST NOT be called while holding s.requestLock OR s.runner.lock.
func (s *InformantServer) RegisterWithInformant(ctx context.Context, logger *zap.Logger) error {
	logger = logger.With(zap.Object("server", s.desc))

	s.requestLock.Lock()
	defer s.requestLock.Unlock()

	// Check the current state:
	err := func() error {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()

		switch s.mode {
		case InformantServerUnconfirmed:
			// good; this is what we're expecting
		case InformantServerRunning, InformantServerSuspended:
			panic(fmt.Errorf("Register called while InformantServer is already registered (mode = %q)", s.mode))
		default:
			panic(fmt.Errorf("Unexpected InformantServerMode %q", s.mode))
		}

		if s.exitStatus != nil {
			err := InformantServerAlreadyExitedError
			s.setLastInformantError(err, true)
			return err
		}

		return nil
	}()
	if err != nil {
		return err
	}

	// Make the request:
	timeout := time.Second * time.Duration(s.runner.global.config.Informant.RegisterTimeoutSeconds)
	resp, statusCode, err := doInformantRequest[api.AgentDesc, api.InformantDesc](
		ctx, logger, s, timeout, http.MethodPost, "/register", &s.desc,
	)
	// Do some stuff with the lock acquired:
	func() {
		maybeMadeContact := statusCode != 0 || ctx.Err() != nil

		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()

		// Record whether we might've contacted the informant:
		s.madeContact = maybeMadeContact

		if err != nil {
			s.setLastInformantError(fmt.Errorf("Register request failed: %w", err), true)

			// If the informant responds that it's our fault, or it had an internal failure, we know
			// that:
			//  1. Neither should happen under normal operation, and
			//  2. Restarting the server is *more likely* to fix it than continuing
			// We shouldn't *assume* that restarting will actually fix it though, so we'll still set
			// RetryShouldFix = false.
			if 400 <= statusCode && statusCode <= 599 {
				s.exit(InformantServerExitStatus{
					Err:            err,
					RetryShouldFix: false,
				})
			}
		}
	}()

	if err != nil {
		return err // the errors returned by doInformantRequest are descriptive enough.
	}

	if err := validateInformantDesc(&s.desc, resp); err != nil {
		err = fmt.Errorf("Received bad InformantDesc: %w", err)
		s.setLastInformantError(err, false)
		return err
	}

	// Now that we know it's valid, set s.runner.informant ...
	err = func() error {
		// ... but only if the server is still current. We're ok setting it if the server isn't
		// running, because it's good to have the information there.
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()

		logger.Info(
			"Informant server mode updated",
			zap.String("action", "register"),
			zap.String("oldMode", string(s.mode)),
			zap.String("newMode", string(InformantServerSuspended)),
		)

		s.mode = InformantServerSuspended
		s.protoVersion = &resp.ProtoVersion

		if s.runner.server == s {
			oldInformant := s.runner.informant
			s.runner.informant = resp
			s.updatedInformant.Send() // signal we've changed the informant

			if oldInformant == nil {
				logger.Info("Registered with informant", zap.Any("informant", *resp))
			} else if *oldInformant != *resp {
				logger.Info(
					"Re-registered with informant, InformantDesc changed",
					zap.Any("oldInformant", *oldInformant),
					zap.Any("informant", *resp),
				)
			} else {
				logger.Info("Re-registered with informant; InformantDesc unchanged", zap.Any("informant", *oldInformant))
			}
		} else {
			logger.Warn("Registering with informant completed but the server has already been replaced")
		}

		// we also want to do a quick protocol check here as well
		if !s.receivedIDCheck {
			// protocol violation
			err := errors.New("Informant responded to /register with 200 without requesting /id")
			s.setLastInformantError(fmt.Errorf("Protocol violation: %w", err), true)
			logger.Error("Protocol violation", zap.Error(err))
			s.exit(InformantServerExitStatus{
				Err:            err,
				RetryShouldFix: false,
			})
			return errors.New("Protocol violation") // we already logged it; don't double-log a long message
		}

		return nil
	}()

	if err != nil {
		return err
	}

	// Record that this request was handled without error
	s.setLastInformantError(nil, false)
	return nil
}

// validateInformantDesc checks that the provided api.InformantDesc is valid and matches with an
// InformantServer's api.AgentDesc
func validateInformantDesc(server *api.AgentDesc, informant *api.InformantDesc) error {
	// To quote the docs for api.InformantDesc.ProtoVersion:
	//
	// > If the VM informant does not use a protocol version within [the agent's] bounds, then it
	// > MUST respond with an error status code.
	//
	// So if we're asked to validate the response, mismatch *should* have already been handled.
	goodProtoVersion := server.MinProtoVersion <= informant.ProtoVersion &&
		informant.ProtoVersion <= server.MaxProtoVersion

	if !goodProtoVersion {
		return fmt.Errorf(
			"Unexpected protocol version: should be between %d and %d, but got %d",
			server.MinProtoVersion, server.MaxProtoVersion, informant.ProtoVersion,
		)
	}

	// To quote the docs for api.InformantMetricsMethod:
	//
	// > At least one method *must* be provided in an InformantDesc, and more than one method gives
	// > the autoscaler-agent freedom to choose.
	//
	// We just need to check that there aren't none.
	hasMetricsMethod := informant.MetricsMethod.Prometheus != nil
	if !hasMetricsMethod {
		return errors.New("No known metrics method given")
	}

	return nil
}

// unregisterFromInformant is an internal-ish function that sends an /unregister request to the VM
// informant
//
// Because sending an /unregister request is generally out of courtesy on exit, this method is more
// permissive about server state, and is typically called with a different Context from what would
// normally be expected.
//
// This method is only expected to be called by s.exit; calling this method before s.exitStatus has
// been set will likely cause the server to restart.
//
// This method MUST NOT be called while holding s.requestLock OR s.runner.lock.
func (s *InformantServer) unregisterFromInformant(ctx context.Context, logger *zap.Logger) error {
	// note: Because this method is typically called during shutdown, we don't set
	// s.runner.lastInformantError or call s.exit, even though other request helpers do.

	logger = logger.With(zap.Object("server", s.desc))

	s.requestLock.Lock()
	defer s.requestLock.Unlock()

	logger.Info("Sending unregister request to informant")

	// Make the request:
	timeout := time.Second * time.Duration(s.runner.global.config.Informant.RegisterTimeoutSeconds)
	resp, _, err := doInformantRequest[api.AgentDesc, api.UnregisterAgent](
		ctx, logger, s, timeout, http.MethodDelete, "/unregister", &s.desc,
	)
	if err != nil {
		return err // the errors returned by doInformantRequest are descriptive enough.
	}

	logger.Info("Unregister request successful", zap.Any("response", *resp))
	return nil
}

// doInformantRequest makes a single HTTP request to the VM informant, doing only the validation
// required to JSON decode the response
//
// The returned int gives the status code of the response. It is possible for a response with status
// 200 to still yield an error - either because of a later IO failure or bad JSON.
//
// If an error occurs before we get a response, the status code will be 0.
//
// This method MUST be called while holding s.requestLock. If not, the program will silently violate
// the protocol guarantees.
func doInformantRequest[Q any, R any](
	ctx context.Context,
	logger *zap.Logger,
	s *InformantServer,
	timeout time.Duration,
	method string,
	path string,
	reqData *Q,
) (_ *R, statusCode int, _ error) {
	result := "<internal error>"
	defer func() {
		s.runner.global.metrics.informantRequestsOutbound.WithLabelValues(path, result).Inc()
	}()

	reqBody, err := json.Marshal(reqData)
	if err != nil {
		return nil, statusCode, fmt.Errorf("Error encoding request JSON: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := s.informantURL(path)
	request, err := http.NewRequestWithContext(reqCtx, method, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, statusCode, fmt.Errorf("Error building request to %q: %w", url, err)
	}
	request.Header.Set("content-type", "application/json")

	logger.Info("Sending informant request", zap.String("url", url), zap.Any("request", reqData))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		result = fmt.Sprintf("[error doing request: %s]", util.RootError(err))
		return nil, statusCode, fmt.Errorf("Error doing request: %w", err)
	}
	defer response.Body.Close()

	statusCode = response.StatusCode
	result = strconv.Itoa(statusCode)

	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, statusCode, fmt.Errorf("Error reading body for response: %w", err)
	}

	if statusCode != 200 {
		return nil, statusCode, fmt.Errorf(
			"Received response status %d body %q", statusCode, string(respBody),
		)
	}

	var respData R
	if err := json.Unmarshal(respBody, &respData); err != nil {
		return nil, statusCode, fmt.Errorf("Bad JSON response: %w", err)
	}

	logger.Info("Got informant response", zap.String("url", url), zap.Any("response", respData))

	return &respData, statusCode, nil
}

// fetchAndIncrementSequenceNumber increments the sequence number and returns it
//
// This method MUST be called while holding s.runner.lock.
func (s *InformantServer) incrementSequenceNumber() uint64 {
	s.seqNum += 1
	return s.seqNum
}

// informantURL creates a string representing the URL for a request to the VM informant, given the
// path to use
func (s *InformantServer) informantURL(path string) string {
	if !strings.HasPrefix(path, "/") {
		panic(errors.New("informant URL path must start with '/'"))
	}

	ip := s.runner.podIP
	port := s.runner.global.config.Informant.ServerPort
	return fmt.Sprintf("http://%s:%d/%s", ip, port, path[1:])
}

// handleID handles a request on the server's /id endpoint. This method should not be called outside
// of that context.
//
// Returns: response body (if successful), status code, error (if unsuccessful)
func (s *InformantServer) handleID(ctx context.Context, _ *zap.Logger, body *struct{}) (_ *api.AgentIdentificationMessage, code int, _ error) {
	defer func() {
		s.runner.global.metrics.informantRequestsInbound.WithLabelValues("/id", strconv.Itoa(code)).Inc()
	}()

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	s.receivedIDCheck = true

	if s.exitStatus != nil {
		return nil, 404, errors.New("Server has already exited")
	}

	// Update our record of the last successful time we heard from the informant, if the server is
	// currently enabled. This allows us to detect cases where the informant is not currently
	// communicating back to the agent - OR when the informant never /resume'd the agent.
	if s.mode == InformantServerRunning {
		s.runner.setStatus(func(s *podStatus) {
			now := time.Now()
			s.lastSuccessfulInformantComm = &now
		})
	}

	return &api.AgentIdentificationMessage{
		Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
		SequenceNumber: s.incrementSequenceNumber(),
	}, 200, nil
}

// handleResume handles a request on the server's /resume endpoint. This method should not be called
// outside of that context.
//
// Returns: response body (if successful), status code, error (if unsuccessful)
func (s *InformantServer) handleResume(
	ctx context.Context, logger *zap.Logger, body *api.ResumeAgent,
) (_ *api.AgentIdentificationMessage, code int, _ error) {
	defer func() {
		s.runner.global.metrics.informantRequestsInbound.WithLabelValues("/resume", strconv.Itoa(code)).Inc()
	}()

	if body.ExpectedID != s.desc.AgentID {
		logger.Warn("Request AgentID not found, server has a different one")
		return nil, 404, fmt.Errorf("AgentID %q not found", body.ExpectedID)
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if s.exitStatus != nil {
		return nil, 404, errors.New("Server has already exited")
	}

	// FIXME: Our handling of the protocol here is racy (because we might receive a /resume request
	// before we've processed the response from our /register request). However, that's *probably*
	// actually an issue with the protocol itself, rather than our handling.

	switch s.mode {
	case InformantServerSuspended:
		s.mode = InformantServerRunning
		logger.Info(
			"Informant server mode updated",
			zap.String("action", "resume"),
			zap.String("oldMode", string(InformantServerSuspended)),
			zap.String("newMode", string(InformantServerRunning)),
		)
	case InformantServerRunning:
		internalErr := errors.New("Got /resume request for server, but it is already running")
		logger.Warn("Protocol violation", zap.Error(internalErr))

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot resume agent that is already running")
	case InformantServerUnconfirmed:
		internalErr := errors.New("Got /resume request for server, but it is unconfirmed")
		logger.Warn("Protocol violation", zap.Error(internalErr))

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot resume agent that is not yet registered")
	default:
		panic(fmt.Errorf("Unexpected InformantServerMode %q", s.mode))
	}

	return &api.AgentIdentificationMessage{
		Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
		SequenceNumber: s.incrementSequenceNumber(),
	}, 200, nil
}

// handleSuspend handles a request on the server's /suspend endpoint. This method should not be
// called outside of that context.
//
// Returns: response body (if successful), status code, error (if unsuccessful)
func (s *InformantServer) handleSuspend(
	ctx context.Context, logger *zap.Logger, body *api.SuspendAgent,
) (_ *api.AgentIdentificationMessage, code int, _ error) {
	defer func() {
		s.runner.global.metrics.informantRequestsInbound.WithLabelValues("/suspend", strconv.Itoa(code)).Inc()
	}()

	if body.ExpectedID != s.desc.AgentID {
		logger.Warn("Request AgentID not found, server has a different one")
		return nil, 404, fmt.Errorf("AgentID %q not found", body.ExpectedID)
	}

	s.runner.lock.Lock()
	locked := true
	defer func() {
		if locked {
			s.runner.lock.Unlock()
		}
	}()

	if s.exitStatus != nil {
		return nil, 404, errors.New("Server has already exited")
	}

	switch s.mode {
	case InformantServerRunning:
		s.mode = InformantServerSuspended
		logger.Info(
			"Informant server mode updated",
			zap.String("action", "suspend"),
			zap.String("oldMode", string(InformantServerRunning)),
			zap.String("newMode", string(InformantServerSuspended)),
		)
	case InformantServerSuspended:
		internalErr := errors.New("Got /suspend request for server, but it is already suspended")
		logger.Warn("Protocol violation", zap.Error(internalErr))

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot suspend agent that is already suspended")
	case InformantServerUnconfirmed:
		internalErr := errors.New("Got /suspend request for server, but it is unconfirmed")
		logger.Warn("Protocol violation", zap.Error(internalErr))

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot suspend agent that is not yet registered")
	}

	locked = false
	s.runner.lock.Unlock()

	// Acquire s.runner.requestLock so that when we return, we can guarantee that any future
	// requests to NeonVM or the scheduler will first observe that the informant is suspended and
	// exit early, before actually making the request.
	if err := s.runner.requestLock.TryLock(ctx); err != nil {
		err = fmt.Errorf("Context expired while trying to acquire requestLock: %w", err)
		logger.Error("Failed to synchronize on requestLock", zap.Error(err))
		return nil, 500, err
	}
	s.runner.requestLock.Unlock() // don't actually hold the lock, we're just using it as a barrier.

	return &api.AgentIdentificationMessage{
		Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
		SequenceNumber: s.incrementSequenceNumber(),
	}, 200, nil
}

// handleTryUpscale handles a request on the server's /try-upscale endpoint. This method should not
// be called outside of that context.
//
// Returns: response body (if successful), status code, error (if unsuccessful)
func (s *InformantServer) handleTryUpscale(
	ctx context.Context,
	logger *zap.Logger,
	body *api.MoreResourcesRequest,
) (_ *api.AgentIdentificationMessage, code int, _ error) {
	defer func() {
		s.runner.global.metrics.informantRequestsInbound.WithLabelValues("/upscale", strconv.Itoa(code)).Inc()
	}()

	if body.ExpectedID != s.desc.AgentID {
		logger.Warn("Request AgentID not found, server has a different one")
		return nil, 404, fmt.Errorf("AgentID %q not found", body.ExpectedID)
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if s.exitStatus != nil {
		return nil, 404, errors.New("Server has already exited")
	}

	switch s.mode {
	case InformantServerRunning:
		if !s.protoVersion.HasTryUpscale() {
			err := fmt.Errorf("/try-upscale not supported for protocol version %v", *s.protoVersion)
			return nil, 400, err
		}

		if body.MoreResources.Cpu || body.MoreResources.Memory {
			s.upscaleRequested.Send()
		} else {
			logger.Warn("Received try-upscale request that has no resources selected")
		}

		logger.Info(
			"Updating requested upscale",
			zap.Any("oldRequested", s.runner.requestedUpscale),
			zap.Any("newRequested", body.MoreResources),
		)
		s.runner.requestedUpscale = body.MoreResources

		return &api.AgentIdentificationMessage{
			Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
			SequenceNumber: s.incrementSequenceNumber(),
		}, 200, nil
	case InformantServerSuspended:
		internalErr := errors.New("Got /try-upscale request for server, but server is suspended")
		logger.Warn("Protocol violation", zap.Error(internalErr))

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot process upscale while suspended")
	case InformantServerUnconfirmed:
		internalErr := errors.New("Got /try-upscale request for server, but server is suspended")
		logger.Warn("Protocol violation", zap.Error(internalErr))

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot process upscale while unconfirmed")
	default:
		panic(fmt.Errorf("unexpected server mode: %q", s.mode))
	}
}

// HealthCheck makes a request to the informant's /health-check endpoint, using the server's ID.
//
// This method MUST be called while holding i.server.requestLock AND NOT i.server.runner.lock.
func (s *InformantServer) HealthCheck(ctx context.Context, logger *zap.Logger) (*api.InformantHealthCheckResp, error) {
	err := func() error {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()
		return s.valid()
	}()
	// NB: we want to continue to perform health checks even if the informant server is not properly
	// available for *normal* use.
	//
	// We only need to check for InformantServerSuspendedError because
	// InformantServerUnconfirmedError will be handled by the retryRegister loop in
	// serveInformantLoop.
	if err != nil && !errors.Is(err, InformantServerSuspendedError) {
		return nil, err
	}

	logger = logger.With(zap.Object("server", s.desc))

	timeout := time.Second * time.Duration(s.runner.global.config.Informant.RequestTimeoutSeconds)
	id := api.AgentIdentification{AgentID: s.desc.AgentID}

	logger.Info("Sending health-check", zap.Any("id", id))
	resp, statusCode, err := doInformantRequest[api.AgentIdentification, api.InformantHealthCheckResp](
		ctx, logger, s, timeout, http.MethodPut, "/health-check", &id,
	)
	if err != nil {
		func() {
			s.runner.lock.Lock()
			defer s.runner.lock.Unlock()

			s.setLastInformantError(fmt.Errorf("Health-check request failed: %w", err), true)

			if 400 <= statusCode && statusCode <= 599 {
				s.exit(InformantServerExitStatus{
					Err:            err,
					RetryShouldFix: statusCode == 404,
				})
			}
		}()
		return nil, err
	}

	logger.Info("Received OK health-check result")
	return resp, nil
}

// Downscale makes a request to the informant's /downscale endpoint with the api.Resources
//
// This method MUST NOT be called while holding i.server.runner.lock OR i.server.requestLock.
func (s *InformantServer) Downscale(ctx context.Context, logger *zap.Logger, to api.Resources) (*api.DownscaleResult, error) {
	s.requestLock.Lock()
	defer s.requestLock.Unlock()

	err := func() error {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()
		return s.valid()
	}()
	if err != nil {
		return nil, err
	}

	logger = logger.With(zap.Object("server", s.desc))

	logger.Info("Sending downscale", zap.Object("target", to))

	timeout := time.Second * time.Duration(s.runner.global.config.Informant.DownscaleTimeoutSeconds)
	id := api.AgentIdentification{AgentID: s.desc.AgentID}
	rawResources := to.ConvertToRaw(s.runner.vm.Mem.SlotSize)

	var statusCode int
	var resp *api.DownscaleResult
	if s.protoVersion.SignsResourceUpdates() {
		signedRawResources := api.ResourceMessage{RawResources: rawResources, Id: id}
		reqData := api.AgentResourceMessage{Data: signedRawResources, SequenceNumber: s.incrementSequenceNumber()}
		resp, statusCode, err = doInformantRequest[api.AgentResourceMessage, api.DownscaleResult](
			ctx, logger, s, timeout, http.MethodPut, "/downscale", &reqData,
		)
	} else {
		resp, statusCode, err = doInformantRequest[api.RawResources, api.DownscaleResult](
			ctx, logger, s, timeout, http.MethodPut, "/downscale", &rawResources,
		)
	}
	if err != nil {
		func() {
			s.runner.lock.Lock()
			defer s.runner.lock.Unlock()

			s.setLastInformantError(fmt.Errorf("Downscale request failed: %w", err), true)

			if 400 <= statusCode && statusCode <= 599 {
				s.exit(InformantServerExitStatus{
					Err:            err,
					RetryShouldFix: statusCode == 404,
				})
			}
		}()
		return nil, err
	}

	logger.Info("Received downscale result") // already logged by doInformantRequest
	return resp, nil
}

func (s *InformantServer) Upscale(ctx context.Context, logger *zap.Logger, to api.Resources) error {
	s.requestLock.Lock()
	defer s.requestLock.Unlock()

	err := func() error {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()
		return s.valid()
	}()
	if err != nil {
		return err
	}

	logger = logger.With(zap.Object("server", s.desc))

	logger.Info("Sending upscale", zap.Object("target", to))

	timeout := time.Second * time.Duration(s.runner.global.config.Informant.DownscaleTimeoutSeconds)
	id := api.AgentIdentification{AgentID: s.desc.AgentID}
	rawResources := to.ConvertToRaw(s.runner.vm.Mem.SlotSize)

	var statusCode int
	if s.protoVersion.SignsResourceUpdates() {
		signedRawResources := api.ResourceMessage{RawResources: rawResources, Id: id}
		reqData := api.AgentResourceMessage{Data: signedRawResources, SequenceNumber: s.incrementSequenceNumber()}
		_, statusCode, err = doInformantRequest[api.AgentResourceMessage, struct{}](
			ctx, logger, s, timeout, http.MethodPut, "/upscale", &reqData,
		)
	} else {
		_, statusCode, err = doInformantRequest[api.RawResources, struct{}](
			ctx, logger, s, timeout, http.MethodPut, "/upscale", &rawResources,
		)
	}
	if err != nil {
		func() {
			s.runner.lock.Lock()
			defer s.runner.lock.Unlock()

			s.setLastInformantError(fmt.Errorf("Downscale request failed: %w", err), true)

			if 400 <= statusCode && statusCode <= 599 {
				s.exit(InformantServerExitStatus{
					Err:            err,
					RetryShouldFix: statusCode == 404,
				})
			}
		}()
		return err
	}

	logger.Info("Received successful upscale result")
	return nil
}
