package agent

// Implementation of the server communicating with VM informants

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const (
	MinInformantProtocolVersion uint32 = 1
	MaxInformantProtocolVersion uint32 = 1
)

func checkInformantProtocolVersion(protoVersion uint32) error {
	good := MinInformantProtocolVersion <= protoVersion && protoVersion <= MaxInformantProtocolVersion
	if good {
		return nil
	}

	return fmt.Errorf(
		"Protocol version mismatch: min = %d, max = %d, but got %d",
		MinInformantProtocolVersion, MaxInformantProtocolVersion, protoVersion,
	)
}

type informantServerState struct {
	desc   api.AgentDesc
	mode   informantServerMode
	seqNum uint64

	lock   sync.Mutex
	logger RunnerLogger

	switchSuspendResume chan<- struct{}

	shutdown context.CancelFunc
	done     util.SignalReceiver

	// ending state of the server. this field MUST NOT be accessed before done.Recv() is closed.
	finalErr error
}

type informantServerMode string

const (
	serverModeSuspended informantServerMode = "suspended"
	serverModeNormal    informantServerMode = "normal"
)

func (r *runner) runInformantServerLoop(
	ctx context.Context,
	logger RunnerLogger,
	panicked chan<- struct{},
	newMetricsPort chan<- uint16,
	notifyNewServer util.CondChannelSender,
	switchSuspendResume chan<- struct{},
) (uint16, error) {
	initialServer, err := startInformantServer(logger, r.thisIP, switchSuspendResume)
	if err != nil {
		return 0, fmt.Errorf("Error starting informant server: %w", err)
	}

	r.informantServer.Store(initialServer)

	// Make sure we shut down the initial server, if we error out early
	started := false
	defer func() {
		if !started {
			initialServer.shutdown()
		}
	}()

	startupTimeout := time.Second * time.Duration(r.config.Informant.MaxStartupSeconds)
	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	metricsPort, err := r.registerWithInformant(startCtx, logger, &initialServer.desc)
	if err != nil {
		return 0, fmt.Errorf("Error registering with VM informant: %w", err)
	}

	started = true
	go func() {
		server := initialServer

		// recover from panics and politely unregister
		defer func() {
			r.informantServer.Store(nil)

			if err := recover(); err != nil {
				fmt.Println(err)
				panicked <- struct{}{}
			}

			if server != nil && ctx.Err() == nil {
				go func() {
					logger.Infof("Sending unregister request to informant")

					reqCtx, cancel := context.WithTimeout(context.TODO(), time.Second)
					defer cancel()

					url := fmt.Sprintf("http://%s:%d/unregister", r.podIP, r.config.Informant.ServerPort)
					body, err := json.Marshal(&server.desc)

					req, err := http.NewRequestWithContext(
						reqCtx, http.MethodDelete, url, bytes.NewReader(body),
					)
					if err != nil {
						klog.Errorf("Error creating unregister request: %s", err)
						return
					}

					client := http.Client{}
					resp, err := client.Do(req)
					if err != nil {
						klog.Errorf("Error sending unregister request: %s", err)
						return
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						klog.Errorf("Unregister request returned non-OK status %d", resp.StatusCode)
						return
					}
				}()
			}
		}()

		defer logger.Infof("Ending VM informant server loop")

		// recreate the server each time we get kicked out
		for {
			err := server.waitUntilFinished(ctx, logger)
			if ctx.Err() != nil {
				return
			}
			r.informantServer.Store(nil)
			server = nil // unset the server so our deferred cleanup won't send an unregister request
			switchSuspendResume <- struct{}{}

			logger.Errorf("Informant server exited with: %s", err)

			delay := time.Second * time.Duration(r.config.Informant.RetryServerAfterSeconds)

			for {
				logger.Infof("Waiting %s before restarting informant server", delay)
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
					logger.Infof("Restarting informant server")
					server, err = startInformantServer(logger, r.thisIP, switchSuspendResume)
					if err != nil {
						logger.Errorf("Error restarting informant server: %s", err)
						delay = time.Second * time.Duration(r.config.Informant.RetryFailedServerDelaySeconds)
						continue
					}

					logger.Infof("Successfully restarted informant server. AgentDesc = %+v", server.desc)

					// Now reconnect with the informant
					//
					// FIXME: ctx has no timeout here, which allows us to loop forever, retrying. We
					// should decide whether we want that behavior.
					newPort, err := r.registerWithInformant(ctx, logger, &server.desc)
					if err != nil {
						logger.Errorf("Error reconnecting to informant: %s", err)
						return // TODO: we should signal error here somehow instead of just returning
					}
					r.informantServer.Store(server)
					notifyNewServer.Send()
					if newPort != metricsPort {
						logger.Infof("Informant metrics port updated %d -> %d", metricsPort, newPort)
						newMetricsPort <- newPort
						metricsPort = newPort
					}
					switchSuspendResume <- struct{}{}
					break
				}
			}
		}
	}()

	return metricsPort, nil
}

func startInformantServer(
	logger RunnerLogger, thisIP string, switchSuspendResume chan<- struct{},
) (*informantServerState, error) {
	addr := net.TCPAddr{IP: net.IPv4zero, Port: 0}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return nil, fmt.Errorf("Error listening on TCP: %w", err)
	}

	sendDone, recvDone := util.NewSingleSignalPair()
	state := &informantServerState{
		desc: api.AgentDesc{
			AgentID:         uuid.New(),
			ServerAddr:      "", // set later
			MinProtoVersion: MinInformantProtocolVersion,
			MaxProtoVersion: MaxInformantProtocolVersion,
		},
		lock:   sync.Mutex{},
		mode:   serverModeSuspended, // State starts suspended, waits for initial Resume
		seqNum: 0,

		switchSuspendResume: switchSuspendResume,

		shutdown: nil, // set later
		done:     recvDone,
		finalErr: nil, // set on server exit
	}

	// Get back the assigned port
	switch addr := listener.Addr().(type) {
	case *net.TCPAddr:
		state.desc.ServerAddr = fmt.Sprintf("%s:%d", thisIP, addr.Port)
	default:
		panic("unexpected net.Addr type")
	}

	mux := http.NewServeMux()
	util.AddHandler(logger.prefix, mux, "/id", http.MethodGet, "struct{}", state.handleID)
	util.AddHandler(logger.prefix, mux, "/resume", http.MethodPost, "ResumeAgent", state.handleResume)
	util.AddHandler(logger.prefix, mux, "/suspend", http.MethodPost, "SuspendAgent", state.handleSuspend)

	server := http.Server{Handler: mux}
	state.shutdown = func() { server.Shutdown(context.TODO()) }

	go func() {
		defer sendDone.Send()
		state.finalErr = server.Serve(listener)
	}()

	return state, nil
}

func (s *informantServerState) getSequenceNumberWhileLocked() uint64 {
	s.seqNum += 1
	return s.seqNum
}

// handleID handles an /id request from a VM informant
//
// Returns: response body (if successful), status code, error (if unsuccessful)
func (s *informantServerState) handleID(body *struct{}) (*api.AgentIdentificationMessage, int, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	return &api.AgentIdentificationMessage{
		Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
		SequenceNumber: s.getSequenceNumberWhileLocked(),
	}, 200, nil
}

// handleResume handles a /resume request from a VM informant
//
// Returns: response body (if successful), status code, error (if unsuccessful)
func (s *informantServerState) handleResume(body *api.ResumeAgent) (*api.AgentIdentificationMessage, int, error) {
	if body.ExpectedID != s.desc.AgentID {
		s.logger.Warningf("AgentID %q not found, server has %q", body.ExpectedID, s.desc.AgentID)
		return nil, 404, fmt.Errorf("AgentID %q not found", body.ExpectedID)
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	switch s.mode {
	case serverModeNormal:
		return nil, 400, fmt.Errorf("cannot resume agent that is already running")
	case serverModeSuspended:
		s.switchSuspendResume <- struct{}{}
		return &api.AgentIdentificationMessage{
			Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
			SequenceNumber: s.getSequenceNumberWhileLocked(),
		}, 200, nil
	default:
		panic(fmt.Sprintf("unknown informantServerMode %q", s.mode))
	}
}

// handleResume handles a /suspend request from a VM informant
//
// Returns: response body (if successful), status code, error (if unsuccessful)
func (s *informantServerState) handleSuspend(body *api.SuspendAgent) (*api.AgentIdentificationMessage, int, error) {
	if body.ExpectedID != s.desc.AgentID {
		s.logger.Warningf("AgentID %q not found, server has %q", body.ExpectedID, s.desc.AgentID)
		return nil, 404, fmt.Errorf("AgentID %q not found", body.ExpectedID)
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	switch s.mode {
	case serverModeNormal:
		s.switchSuspendResume <- struct{}{}
		return &api.AgentIdentificationMessage{
			Data:           api.AgentIdentification{AgentID: s.desc.AgentID},
			SequenceNumber: s.getSequenceNumberWhileLocked(),
		}, 200, nil
	case serverModeSuspended:
		return nil, 400, fmt.Errorf("cannot suspend agent that is already running")
	default:
		panic(fmt.Sprintf("unknown informantServerMode %q", s.mode))
	}
}

func (r *runner) registerWithInformant(
	ctx context.Context, logger RunnerLogger, serverDesc *api.AgentDesc,
) (uint16, error) {
	requestTimeout := time.Second * time.Duration(r.config.Informant.RequestTimeoutSeconds)

	reqBody, err := json.Marshal(serverDesc)
	if err != nil {
		panic(fmt.Sprintf("Error creating AgentDesc JSON: %s", err))
	}

	url := fmt.Sprintf("http://%s:%d/register", r.podIP, r.config.Informant.ServerPort)

	client := http.Client{}

	for {
		// Use a separate function for each so that we can cancel()s the contexts for each request
		// before the outer loop ends.
		done, metricsPort, err := func() (bool, uint16, error) {
			reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(reqBody))
			if err != nil {
				return true, 0, fmt.Errorf("Error constructing register request: %w", err)
			}

			resp, err := client.Do(req)
			if err != nil {
				return false, 0, fmt.Errorf("Error executing request: %w", err)
			}
			defer resp.Body.Close()

			// At this point, we've gotten *some* kind of response. If there's a problem with
			// it, we'll assume that retrying won't help.
			if resp.StatusCode != http.StatusOK {
				return true, 0, fmt.Errorf("Unsuccessful informant status %d", resp.StatusCode)
			}

			var informantDesc api.InformantDesc
			if err = json.NewDecoder(resp.Body).Decode(&informantDesc); err != nil {
				return true, 0, fmt.Errorf("Error parsing informant response JSON: %w", err)
			}

			metricsPort, err := func() (uint16, error) {
				if err := checkInformantProtocolVersion(informantDesc.ProtoVersion); err != nil {
					return 0, err
				}

				switch {
				case informantDesc.MetricsMethod.Prometheus != nil:
					return informantDesc.MetricsMethod.Prometheus.Port, nil
				default:
					return 0, fmt.Errorf("No metrics method given")
				}
			}()

			if err != nil {
				return true, 0, fmt.Errorf("Bad response from informant: %w", err)
			}

			return true, metricsPort, nil
		}()

		if err == nil {
			logger.Infof("Successfully registered with VM informant, desc = %+v", serverDesc)
			return metricsPort, nil
		}

		if done {
			err = fmt.Errorf("Failed to register with VM informant: %w", err)
			logger.Errorf("%s", err)
			logger.Infof("Ending VM informant connection attempts")
			return 0, err
		} else if ce := ctx.Err(); ce != nil {
			logger.Infof("Ending VM informant connection attempts: %s", ce)
			return 0, ce
		} else {
			retryAfter := time.Second * time.Duration(r.config.Informant.RetryRegisterAfterSeconds)
			logger.Warningf("Error registering with VM informant: %s", err)
			logger.Infof("Retrying VM informant after %s", retryAfter)
			select {
			case <-ctx.Done():
				err := ctx.Err()
				logger.Infof("Skipping retry, context ended: %s", err)
				return 0, err
			case <-time.After(retryAfter):
				continue
			}
		}
	}
}

// always returns a non-nil error
func (s *informantServerState) waitUntilFinished(
	ctx context.Context, logger RunnerLogger,
) error {
	select {
	case <-ctx.Done():
		s.shutdown()
		<-s.done.Recv()
		return s.finalErr
	case <-s.done.Recv():
		return s.finalErr
	}
}
