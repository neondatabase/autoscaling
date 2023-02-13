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
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// The autoscaler-agent currently supports v1.0 to v1.1 of the agent<->informant protocol.
//
// If you update either of these values, make sure to also update VERSIONING.md.
const (
	MinInformantProtocolVersion api.InformantProtoVersion = api.InformantProtoV1_0
	MaxInformantProtocolVersion api.InformantProtoVersion = api.InformantProtoV1_1
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
	// runner.lock. Similarly, if both requestLock and runner.vmStateLock are required, then
	// runner.vmStateLock MUST be acquired before requestLock.
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
	// Err is the non-nil error that caused the server to exit
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
	runner *Runner,
	updatedInformant util.CondChannelSender,
	upscaleRequested util.CondChannelSender,
) (*InformantServer, util.SignalReceiver, error) {
	// Manually start the TCP listener so that we can see the port it's assigned
	addr := net.TCPAddr{IP: net.IPv4zero, Port: 0 /* 0 means it'll be assigned any(-ish) port */}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return nil, util.SignalReceiver{}, fmt.Errorf("Error listening on TCP: %w", err)
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

	runner.logger.Infof("Starting Informant server, desc = %+v", server.desc)

	logPrefix := runner.logger.prefix

	mux := http.NewServeMux()
	util.AddHandler(logPrefix, mux, "/id", http.MethodGet, "struct{}", server.handleID)
	util.AddHandler(logPrefix, mux, "/resume", http.MethodPost, "ResumeAgent", server.handleResume)
	util.AddHandler(logPrefix, mux, "/suspend", http.MethodPost, "SuspendAgent", server.handleSuspend)
	util.AddHandler(logPrefix, mux, "/try-upscale", http.MethodPost, "MoreResourcesRequest", server.handleTryUpscale)
	httpServer := &http.Server{Handler: mux}

	sendFinished, recvFinished := util.NewSingleSignalPair()
	backgroundCtx, cancelBackground := context.WithCancel(ctx)

	// note: docs for server.exit guarantee this function is called while holding runner.lock.
	server.exit = func(status InformantServerExitStatus) {
		sendFinished.Send()
		cancelBackground()

		// Set server.exitStatus if isn't already
		if server.exitStatus == nil {
			server.exitStatus = &status
			logFunc := runner.logger.Warningf
			if status.RetryShouldFix {
				logFunc = runner.logger.Infof
			}

			logFunc("Informant server exiting with retry: %v, err: %s", status.RetryShouldFix, status.Err)
		}

		shutdownName := fmt.Sprintf("InformantServer shutdown (%s)", server.desc.AgentID)

		// we need to spawn these in separate threads so the caller doesn't block while holding
		// runner.lock
		runner.spawnBackgroundWorker(context.TODO(), shutdownName, func(c context.Context) {
			if err := httpServer.Shutdown(c); err != nil {
				runner.logger.Warningf("Error shutting down InformantServer: %s", err)
			}
		})
		if server.madeContact {
			// only unregister the server if we could have plausibly contacted the informant
			unregisterName := fmt.Sprintf("InformantServer unregister (%s)", server.desc.AgentID)
			runner.spawnBackgroundWorker(context.TODO(), unregisterName, func(c context.Context) {
				if err := server.unregisterFromInformant(c); err != nil {
					runner.logger.Warningf("Error unregistering %s: %s", server.desc.AgentID, err)
				}
			})
		}
	}

	// Deadlock checker for server.requestLock
	//
	// FIXME: make these timeouts/delays separately defined constants, or configurable
	deadlockChecker := makeDeadlockChecker(&server.requestLock, 5*time.Second, time.Second)
	deadlockWorkerName := fmt.Sprintf("InformantServer deadlock checker (%s)", server.desc.AgentID)
	runner.spawnBackgroundWorker(backgroundCtx, deadlockWorkerName, deadlockChecker)

	// Main thread running the server. After httpServer.Serve() completes, we do some error
	// handling, but that's about it.
	serverName := fmt.Sprintf("InformantServer (%s)", server.desc.AgentID)
	runner.spawnBackgroundWorker(ctx, serverName, func(c context.Context) {
		if err := httpServer.Serve(listener); errors.Is(err, http.ErrServerClosed) {
			runner.logger.Errorf("InformantServer exited with unexpected error: %s", err)
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

// Valid checks if the InformantServer is good to use for communication, returning an error if not
//
// This method can return errors for a number of unavoidably-racy protocol states - errors from this
// method should be handled as unusual, but not unexpected. Any error returned will be one of
// InformantServer{AlreadyExited,Suspended,Confirmed}Error.
//
// This method MUST be called while holding s.runner.lock.
func (s *InformantServer) Valid() error {
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
func (s *InformantServer) RegisterWithInformant(ctx context.Context) error {
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
		ctx, s, timeout, http.MethodPost, "/register", &s.desc,
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

		s.runner.logger.Infof(
			"Informant server registered, mode: %q -> %q", s.mode, InformantServerSuspended,
		)

		s.mode = InformantServerSuspended
		s.protoVersion = &resp.ProtoVersion

		if s.runner.server == s {
			oldInformant := s.runner.informant
			s.runner.informant = resp
			s.updatedInformant.Send() // signal we've changed the informant

			if oldInformant == nil {
				s.runner.logger.Infof("Registered with informant, InformantDesc is %+v", *resp)
			} else if *oldInformant == *resp {
				s.runner.logger.Infof(
					"Re-registered with informant, InformantDesc changed from %+v to %+v",
					*oldInformant, *resp,
				)
			} else {
				s.runner.logger.Infof("Re-registered with informant; InformantDesc unchanged")
			}
		} else {
			s.runner.logger.Warningf(
				"Registering server %s completed but there is a new server now", s.desc.AgentID,
			)
		}

		// we also want to do a quick protocol check here as well
		if !s.receivedIDCheck {
			// protocol violation
			err := errors.New("Protocol violation: Informant responded to /register with 200 without requesting /id")
			s.setLastInformantError(err, true)
			s.runner.logger.Errorf("%s", err)
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
func (s *InformantServer) unregisterFromInformant(ctx context.Context) error {
	// note: Because this method is typically called during shutdown, we don't set
	// s.runner.lastInformantError or call s.exit, even though other request helpers do.

	s.requestLock.Lock()
	defer s.requestLock.Unlock()

	s.runner.logger.Infof("Sending unregister %s request to informant", s.desc.AgentID)

	// Make the request:
	timeout := time.Second * time.Duration(s.runner.global.config.Informant.RegisterTimeoutSeconds)
	resp, _, err := doInformantRequest[api.AgentDesc, api.UnregisterAgent](
		ctx, s, timeout, http.MethodDelete, "/unregister", &s.desc,
	)
	if err != nil {
		return err // the errors returned by doInformantRequest are descriptive enough.
	}

	s.runner.logger.Infof("Unregister %s request successful: %+v", *resp)
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
	s *InformantServer,
	timeout time.Duration,
	method string,
	path string,
	reqData *Q,
) (_ *R, statusCode int, _ error) {
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

	s.runner.logger.Infof("Sending informant %q request: %s", path, string(reqBody))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, statusCode, fmt.Errorf("Error doing request: %w", err)
	}
	defer response.Body.Close()

	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, statusCode, fmt.Errorf("Error reading body for response: %w", err)
	}

	statusCode = response.StatusCode

	if statusCode != 200 {
		return nil, statusCode, fmt.Errorf(
			"Received response status %d body %q", statusCode, string(respBody),
		)
	}

	var respData R
	if err := json.Unmarshal(respBody, &respData); err != nil {
		return nil, statusCode, fmt.Errorf("Bad JSON response: %w", err)
	}

	s.runner.logger.Infof("Got informant %q response: %s", path, string(respBody))

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
func (s *InformantServer) handleID(body *struct{}) (*api.AgentIdentificationMessage, int, error) {
	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	s.receivedIDCheck = true

	if s.exitStatus != nil {
		return nil, 404, errors.New("Server has already exited")
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
func (s *InformantServer) handleResume(body *api.ResumeAgent) (*api.AgentIdentificationMessage, int, error) {
	if body.ExpectedID != s.desc.AgentID {
		s.runner.logger.Warningf("AgentID %q not found, server has %q", body.ExpectedID, s.desc.AgentID)
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
		s.runner.logger.Infof(
			"Informant server mode: %q -> %q", InformantServerSuspended, InformantServerRunning,
		)
	case InformantServerRunning:
		internalErr := errors.New("Got /resume request for server, but it is already running")
		s.runner.logger.Warningf("%s", internalErr)

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot resume agent that is already running")
	case InformantServerUnconfirmed:
		internalErr := errors.New("Got /resume request for server, but it is unconfirmed")
		s.runner.logger.Warningf("%s", internalErr)

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
func (s *InformantServer) handleSuspend(body *api.SuspendAgent) (*api.AgentIdentificationMessage, int, error) {
	if body.ExpectedID != s.desc.AgentID {
		s.runner.logger.Warningf("AgentID %q not found, server has %q", body.ExpectedID, s.desc.AgentID)
		return nil, 404, fmt.Errorf("AgentID %q not found", body.ExpectedID)
	}

	s.runner.lock.Lock()
	defer s.runner.lock.Unlock()

	if s.exitStatus != nil {
		return nil, 404, errors.New("Server has already exited")
	}

	switch s.mode {
	case InformantServerRunning:
		s.mode = InformantServerSuspended
		s.runner.logger.Infof(
			"Informant server mode: %q -> %q", InformantServerRunning, InformantServerSuspended,
		)
	case InformantServerSuspended:
		internalErr := errors.New("Got /suspend request for server, but it is already suspended")
		s.runner.logger.Warningf("%s", internalErr)

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot suspend agent that is already suspended")
	case InformantServerUnconfirmed:
		internalErr := errors.New("Got /suspend request for server, but it is unconfirmed")
		s.runner.logger.Warningf("%s", internalErr)

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot suspend agent that is not yet registered")
	}

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
	body *api.MoreResourcesRequest,
) (*api.AgentIdentificationMessage, int, error) {
	if body.ExpectedID != s.desc.AgentID {
		s.runner.logger.Warningf("AgentID %q not found, server has %q", body.ExpectedID, s.desc.AgentID)
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
			s.runner.logger.Warningf("Received try-upscale request that does not increase")
		}

		s.runner.logger.Infof(
			"Updating requested upscale from %+v to %+v",
			s.runner.requestedUpscale, body.MoreResources,
		)
		s.runner.requestedUpscale = body.MoreResources

		return &api.AgentIdentificationMessage{
			Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
			SequenceNumber: s.incrementSequenceNumber(),
		}, 200, nil
	case InformantServerSuspended:
		internalErr := errors.New("Got /try-upscale request for server, but server is suspended")
		s.runner.logger.Warningf("%s", internalErr)

		// To be nice, we'll restart the server. We don't want to make a temporary error permanent.
		s.exit(InformantServerExitStatus{
			Err:            internalErr,
			RetryShouldFix: true,
		})

		return nil, 400, errors.New("Cannot process upscale while suspended")
	case InformantServerUnconfirmed:
		internalErr := errors.New("Got /try-upscale request for server, but server is suspended")
		s.runner.logger.Warningf("%s", internalErr)

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

// Downscale makes a request to the informant's /downscale endpoit with the api.Resources
//
// This method MUST NOT be called while holding i.server.runner.lock OR i.server.requestLock.
func (s *InformantServer) Downscale(ctx context.Context, to api.Resources) (*api.DownscaleResult, error) {
	s.requestLock.Lock()
	defer s.requestLock.Unlock()

	err := func() error {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()
		return s.Valid()
	}()
	if err != nil {
		return nil, err
	}

	s.runner.logger.Infof("Sending downscale %+v", to)

	timeout := time.Second * time.Duration(s.runner.global.config.Informant.DownscaleTimeoutSeconds)
	rawResources := to.ConvertToRaw(&s.runner.vm.Mem.SlotSize)

	resp, statusCode, err := doInformantRequest[api.RawResources, api.DownscaleResult](
		ctx, s, timeout, http.MethodPut, "/downscale", &rawResources,
	)
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

	s.runner.logger.Infof("Received downscale result %+v", *resp)
	return resp, nil
}

func (s *InformantServer) Upscale(ctx context.Context, to api.Resources) error {
	s.requestLock.Lock()
	defer s.requestLock.Unlock()

	err := func() error {
		s.runner.lock.Lock()
		defer s.runner.lock.Unlock()
		return s.Valid()
	}()
	if err != nil {
		return err
	}

	s.runner.logger.Infof("Sending upscale %+v", to)

	timeout := time.Second * time.Duration(s.runner.global.config.Informant.DownscaleTimeoutSeconds)
	rawResources := to.ConvertToRaw(&s.runner.vm.Mem.SlotSize)

	_, statusCode, err := doInformantRequest[api.RawResources, struct{}](
		ctx, s, timeout, http.MethodPut, "/upscale", &rawResources,
	)
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

	s.runner.logger.Infof("Received successful upscale result")
	return nil
}
