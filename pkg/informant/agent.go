package informant

// This file contains the "client" methods for communicating with an autoscaler-agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/exp/slices"

	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

// The VM informant currently supports v1.1 and v1.2 of the agent<->informant protocol.
//
// If you update either of these values, make sure to also update VERSIONING.md.
const (
	MinProtocolVersion api.InformantProtoVersion = api.InformantProtoV1_1
	MaxProtocolVersion api.InformantProtoVersion = api.InformantProtoV1_2
)

// AgentSet is the global state handling various autoscaler-agents that we could connect to
type AgentSet struct {
	lock sync.Mutex

	// current is the agent we're currently communciating with. If there is none, then this value is
	// nil
	//
	// This value may (temporarily) be nil even when there are other agents waiting in byIDs/byTime,
	// because we rely on tryNewAgents to handle setting the value here.
	current *Agent

	// wantsMemoryUpscale is true if the most recent (internal) request for immediate upscaling has
	// not yet been answered (externally) by notification of an upscale from the autoscaler-agent.
	wantsMemoryUpscale bool

	// byIDs stores all of the agents, indexed by their unique IDs
	byIDs map[uuid.UUID]*Agent
	// byTime stores all of the *successfully registered* agents, sorted in increasing order of
	// their initial /register request. Agents that we're currently in the process of handling will
	// be present in byIDs, but not here.
	byTime []*Agent

	tryNewAgent chan<- struct{}
}

type Agent struct {
	// lock is required for accessing the mutable fields of this struct: parent and lastSeqNumber.
	lock sync.Mutex

	// parent is the AgentSet containing this Agent. It is always non-nil, up until this Agent is
	// unregistered with EnsureUnregistered()
	parent *AgentSet

	// suspended is true if this Agent was last sent a request on /suspend. This is only ever set by
	suspended bool

	// unregistered signalled when the agent is unregistered (due to an error or an /unregister
	// request)
	unregistered util.SignalReceiver
	// Sending half of unregistered — only used by EnsureUnregistered()
	signalUnregistered util.SignalSender

	id         uuid.UUID
	serverAddr string

	protoVersion api.InformantProtoVersion

	// all sends on requestQueue are made through the doRequest method; all receives are made from
	// the runHandler background task.
	requestQueue  chan agentRequest
	lastSeqNumber uint64
}

type agentRequest struct {
	ctx       context.Context
	done      util.SignalSender
	doRequest func(context.Context, *http.Client)
}

// NewAgentSet creates a new AgentSet and starts the necessary background tasks
//
// On completion, the background tasks should be ended with the Stop method.
func NewAgentSet() *AgentSet {
	tryNewAgent := make(chan struct{})

	agents := &AgentSet{
		lock:               sync.Mutex{},
		current:            nil,
		wantsMemoryUpscale: false,
		byIDs:              make(map[uuid.UUID]*Agent),
		byTime:             []*Agent{},
		tryNewAgent:        tryNewAgent,
	}

	go agents.runDeadlockChecker()
	go agents.tryNewAgents(tryNewAgent)
	return agents
}

// runDeadlockChecker periodically tries to acquire the AgentSet's lock, killing the process if it
// was unable to do so
func (s *AgentSet) runDeadlockChecker() {
	for {
		success := make(chan struct{})

		go func() {
			s.lock.Lock()
			defer s.lock.Unlock()
			close(success)
		}()

		select {
		case <-success:
			// all good
		case <-time.After(CheckDeadlockTimeout):
			klog.Fatalf("deadlock detected, could not get lock after %s", CheckDeadlockTimeout)
		}

		time.Sleep(CheckDeadlockDelay)
	}
}

func (s *AgentSet) tryNewAgents(signal <-chan struct{}) {
	// note: we don't close this. Sending stops when the context is done, and every read from this
	// channel also handles the context being cancelled.
	aggregate := make(chan struct{})

	// Helper function to coalesce repeated incoming signals into a single output, so that we don't
	// block anything from sending on signal
	go func() {
	noSignal:
		<-signal

	yesSignal:
		select {
		case <-signal:
			goto yesSignal
		case aggregate <- struct{}{}:
			goto noSignal
		}
	}()

	for {
		<-aggregate

		// Loop through applicable Agents
	loopThroughAgents:
		for {
			// Remove any duplicate signals from aggregate if there are any
			select {
			case <-aggregate:
			default:
			}

			candidate := func() *Agent {
				s.lock.Lock()
				defer s.lock.Unlock()

				if len(s.byTime) == 0 || s.current == s.byTime[len(s.byTime)-1] {
					return nil
				}

				return s.byTime[len(s.byTime)-1]
			}()

			// If there's no remaining candidates, stop trying.
			if candidate == nil {
				break loopThroughAgents
			}

			// Do we need to resume the agent? We will use this later
			shouldResume := func() bool {
				candidate.lock.Lock()
				defer candidate.lock.Unlock()

				wasSuspended := candidate.suspended
				candidate.suspended = false
				return !wasSuspended
			}()

			// Get the current agent, which we would like to replace with the candidate.
			// We should suspend the old agent.
			oldCurrent := func() (old *Agent) {
				s.lock.Lock()
				defer s.lock.Unlock()

				if s.current != nil {
					s.current.suspended = true
				}

				return s.current
			}()

			if oldCurrent != nil {
				handleError := func(err error) {
					if errors.Is(err, context.Canceled) {
						return
					}

					klog.Warningf(
						"Error suspending previous Agent %s/%s: %s",
						oldCurrent.serverAddr, oldCurrent.id, err,
					)
				}

				// Suspend the old agent
				oldCurrent.Suspend(AgentSuspendTimeout, handleError)
			}

			if shouldResume {
				if err := candidate.Resume(AgentResumeTimeout); err != nil {
					// From Resume():
					//
					// > If the Agent becomes unregistered [ ... ] this method will return
					// > context.Canceled
					if err == context.Canceled { //nolint:errorlint // explicit error value guarantee from Resume()
						continue loopThroughAgents
					}

					// From Resume():
					//
					// > If the request fails, the Agent will be unregistered
					//
					// We don't have to worry about anything extra here; just keep trying.
					if err != nil {
						klog.Warningf(
							"Error on Resume for agent %s/%s: %s",
							candidate.serverAddr, candidate.id, err,
						)
						continue loopThroughAgents
					}
				}
			}

			// Set the new agent, and do an upscale if it was requested.
			func() {
				s.lock.Lock()
				defer s.lock.Unlock()

				s.current = candidate

				if s.wantsMemoryUpscale {
					s.current.SpawnRequestUpscale(AgentUpscaleTimeout, func(err error) {
						if errors.Is(err, context.Canceled) {
							return
						}

						// note: explicitly refer to candidate here instead of s.current, because
						// the value of s.current could have changed by the time this function is
						// called.
						klog.Errorf(
							"Error requesting upscale from Agent %s/%s: %s",
							candidate.serverAddr, candidate.id, err,
						)
					})
				}
			}()
		}
	}
}

// RegisterNewAgent instantiates our local information about the autsocaler-agent
//
// Returns: protocol version, status code, error (if unsuccessful)
func (s *AgentSet) RegisterNewAgent(info *api.AgentDesc) (api.InformantProtoVersion, int, error) {
	expectedRange := api.VersionRange[api.InformantProtoVersion]{
		Min: MinProtocolVersion,
		Max: MaxProtocolVersion,
	}

	descProtoRange := info.ProtocolRange()

	protoVersion, matches := expectedRange.LatestSharedVersion(descProtoRange)
	if !matches {
		return 0, 400, fmt.Errorf(
			"Protocol version mismatch: Need %v but got %v", expectedRange, descProtoRange,
		)
	}

	unregisterSend, unregisterRecv := util.NewSingleSignalPair()

	agent := &Agent{
		lock: sync.Mutex{},

		parent: s,

		suspended:          false,
		unregistered:       unregisterRecv,
		signalUnregistered: unregisterSend,

		id:         info.AgentID,
		serverAddr: info.ServerAddr,

		protoVersion: protoVersion,

		lastSeqNumber: 0,
		requestQueue:  make(chan agentRequest),
	}

	// Try to add the agent, if we can.
	isDuplicate := func() bool {
		s.lock.Lock()
		defer s.lock.Unlock()

		if _, ok := s.byIDs[info.AgentID]; ok {
			return true
		}

		s.byIDs[info.AgentID] = agent
		return false
	}()

	if isDuplicate {
		return 0, 409, fmt.Errorf("Agent with ID %s is already registered", info.AgentID)
	}

	go agent.runHandler()
	go agent.runBackgroundChecker()

	if err := agent.CheckID(AgentBackgroundCheckTimeout); err != nil {
		return 0, 400, fmt.Errorf(
			"Error checking ID for agent %s/%s: %w", agent.serverAddr, agent.id, err,
		)
	}

	// note: At this point, the agent has been appropriately established, but we haven't added it to
	// the AgentSet's list of successfully registered Agents
	func() {
		// We have to acquire a lock on the Agent state here so that we don't have a race from a
		// concurrent call to EnsureUnregistered().
		agent.lock.Lock()
		defer agent.lock.Unlock()

		if agent.parent == nil {
			// Something caused the Agent to be unregistered. We don't know what, but it wasn't the
			// fault of this request. Because there's no strict happens-before relation here, we can
			// pretend like the error happened after the request was fully handled, and return a
			// success.
			klog.Warningf(
				"Agent %s/%s was unregistered before register was completed",
				agent.serverAddr, agent.id,
			)
			return
		}

		s.lock.Lock()
		defer s.lock.Unlock()

		s.byTime = append(s.byTime, agent)
		s.tryNewAgent <- struct{}{}
	}()

	return protoVersion, 200, nil
}

// RequestUpscale requests an immediate upscale for more memory, if there's an agent currently
// enabled
//
// If there's no current agent, then RequestUpscale marks the upscale as desired, and will request
// upscaling from the next agent we connect to.
func (s *AgentSet) RequestUpscale() {
	// FIXME: we should assign a timeout to these upscale requests, so that we don't continue trying
	// to upscale after the demand has gone away.

	agent := func() *Agent {
		s.lock.Lock()
		defer s.lock.Unlock()

		// If we already have an ongoing request, don't create a new one.
		if s.wantsMemoryUpscale {
			return nil
		}

		s.wantsMemoryUpscale = true
		return s.current
	}()

	if agent == nil {
		return
	}

	// FIXME: it's possible to block for an unbounded amount of time waiting for the request to get
	// picked up by the message queue. We *do* want backpressure here, but we should ideally have a
	// way to cancel an attempted request if it's taking too long.
	agent.SpawnRequestUpscale(AgentUpscaleTimeout, func(err error) {
		if errors.Is(err, context.Canceled) {
			return
		}

		klog.Errorf(
			"Error requesting upscale from Agent %s/%s: %s",
			agent.serverAddr, agent.id, err,
		)
	})
}

// ReceivedUpscale marks any desired upscaling from a prior s.RequestUpscale() as resolved
//
// Typically, (*CgroupState).ReceivedUpscale() is also called alongside this method.
func (s *AgentSet) ReceivedUpscale() {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.wantsMemoryUpscale = false
}

// Get returns the requested Agent, if it exists
func (s *AgentSet) Get(id uuid.UUID) (_ *Agent, ok bool) {
	s.lock.Lock()
	defer s.lock.Unlock()

	agent, ok := s.byIDs[id]
	return agent, ok
}

// runHandler receives inputs from the requestSet and dispatches them
func (a *Agent) runHandler() {
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := fmt.Errorf("Unexpected redirect getting %s", req.URL)
			klog.Warningf("%s", err)
			return err
		},
	}

	defer client.CloseIdleConnections()

	for {
		// Ignore items in the requestQueue if the Agent's been unregistered.
		select {
		case <-a.unregistered.Recv():
			return
		default:
		}

		select {
		case <-a.unregistered.Recv():
			return
		case req := <-a.requestQueue:
			func() {
				reqCtx, cancel := context.WithCancel(req.ctx)
				defer cancel()

				done := make(chan struct{})
				go func() {
					defer req.done.Send()
					defer close(done)
					req.doRequest(reqCtx, &client)
				}()

				select {
				case <-a.unregistered.Recv():
					cancel()
					// Even if we've just cancelled it, we have to wait on done so that we know the
					// http.Client won't be used by other goroutines
					<-done
				case <-done:
				}
			}()
		}
	}
}

// runBackgroundChecker performs periodic checks that the Agent is still available
func (a *Agent) runBackgroundChecker() {
	for {
		select {
		case <-a.unregistered.Recv():
			return
		case <-time.After(AgentBackgroundCheckDelay):
			// all good
		}

		done := func() bool {
			if err := a.CheckID(AgentBackgroundCheckTimeout); err != nil {
				// If this request was cancelled (because the agent was unregistered), we're done.
				// We can't check a.unregistered because CheckID will already unregister on failure
				// anyways.
				if errors.Is(err, context.Canceled) {
					return true
				}

				klog.Warningf("Agent ID background check failed for %s/%s: %s", a.serverAddr, a.id, err)
				return true
			}

			return false
		}()

		if done {
			return
		}
	}
}

// doRequest is the generic wrapper around requests to the autoscaler-agent to ensure that we're
// only sending one at a time AND we appropriately keep track of sequence numbers.
//
// We can only send one at a time because http.Client isn't thread-safe, and we want to re-use it
// between requests so that we can keep the TCP connections alive.
//
// There are no guarantees made about the equality or content of errors returned from this function.
func doRequest[B any, R any](
	agent *Agent,
	timeout time.Duration,
	method string,
	path string,
	body *B,
) (_ *R, old bool, _ error) {
	return doRequestWithStartSignal[B, R](
		agent, timeout, nil, method, path, body,
	)
}

func doRequestWithStartSignal[B any, R any](
	agent *Agent,
	timeout time.Duration,
	start *util.SignalSender,
	method string,
	path string,
	body *B,
) (_ *R, old bool, _ error) {
	outerContext, cancel := context.WithTimeout(context.TODO(), timeout)
	defer cancel()

	var (
		responseBody api.AgentMessage[R]
		oldSeqNum    bool
		requestErr   error
	)

	sendDone, recvDone := util.NewSingleSignalPair()

	url := fmt.Sprintf("http://%s%s", agent.serverAddr, path)

	req := agentRequest{
		ctx:  outerContext,
		done: sendDone,
		doRequest: func(ctx context.Context, client *http.Client) {
			bodyBytes, err := json.Marshal(body)
			if err != nil {
				requestErr = fmt.Errorf("Error encoding JSON body: %w", err)
				return
			}

			req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(bodyBytes))
			if err != nil {
				requestErr = fmt.Errorf("Error creating request: %w", err)
				return
			}

			klog.Infof("Sending agent %s %q request: %s", agent.id, path, string(bodyBytes))

			resp, err := client.Do(req)
			if err != nil {
				requestErr = err
				return
			}

			defer resp.Body.Close()

			respBodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				requestErr = fmt.Errorf("Error reading response body: %w", err)
				return
			}
			if resp.StatusCode != 200 {
				requestErr = fmt.Errorf(
					"Unsuccessful response status %d: %s",
					resp.StatusCode, string(respBodyBytes),
				)
				return
			}
			if err := json.Unmarshal(respBodyBytes, &responseBody); err != nil {
				requestErr = fmt.Errorf("Error reading response as JSON: %w", err)
				return
			}

			klog.Infof("Got agent %s response: %s", agent.id, string(respBodyBytes))

			if responseBody.SequenceNumber == 0 {
				requestErr = errors.New("Got invalid sequence number 0")
				return
			}

			// Acquire the Agent's lock so we can check the sequence number
			agent.lock.Lock()
			defer agent.lock.Unlock()

			if agent.lastSeqNumber < responseBody.SequenceNumber {
				agent.lastSeqNumber = responseBody.SequenceNumber
			} else {
				oldSeqNum = true
			}
		},
	}

	// Try to queue the request
	select {
	case <-outerContext.Done():
		// Timeout reached
		return nil, false, outerContext.Err()
	case <-agent.unregistered.Recv():
		return nil, false, context.Canceled
	case agent.requestQueue <- req:
		// Continue as normal
	}

	if start != nil {
		start.Send()
	}

	// At this point, runHandler is appropriately handling the request, and will call
	// sendDone.Send() the attempt at the request is finished. We don't need to worry about handling
	// timeouts & unregistered Agents ourselves.
	<-recvDone.Recv()

	if requestErr != nil {
		return nil, oldSeqNum, requestErr
	} else {
		return &responseBody.Data, oldSeqNum, nil
	}
}

// EnsureUnregistered unregisters the Agent if it is currently registered, signalling the AgentSet
// to use a new Agent if it isn't already
//
// Returns whether the agent was the current Agent in use.
func (a *Agent) EnsureUnregistered() (wasCurrent bool) {
	a.lock.Lock()
	defer a.lock.Unlock()

	if a.parent == nil {
		return
	}

	klog.Infof("Unregistering agent %s/%s", a.serverAddr, a.id)

	a.signalUnregistered.Send()

	a.parent.lock.Lock()
	defer a.parent.lock.Unlock()

	if _, ok := a.parent.byIDs[a.id]; ok {
		delete(a.parent.byIDs, a.id)
	} else {
		klog.Errorf(
			"Invalid state: agent %s/%s is registered but not in parent's agents map. Ignoring and continuing.",
			a.serverAddr, a.id,
		)
	}

	if idx := slices.Index(a.parent.byTime, a); idx >= 0 {
		a.parent.byTime = slices.Delete(a.parent.byTime, idx, idx+1)
	}

	if a.parent.current == a {
		wasCurrent = true
		a.parent.current = nil
		a.parent.tryNewAgent <- struct{}{}
	}

	a.parent = nil

	return
}

// CheckID checks that the Agent's ID matches what's expected
//
// If the agent has already been registered, then a failure in this method will unregister the
// agent.
//
// If the Agent is unregistered before the call to CheckID() completes, the request will be cancelled
// and this method will return context.Canceled.
func (a *Agent) CheckID(timeout time.Duration) error {
	// Quick unregistered check:
	select {
	case <-a.unregistered.Recv():
		klog.Warningf(
			"CheckID called for Agent %s/%s that is already unregistered (probably *not* a race?)",
			a.serverAddr, a.id,
		)
		return context.Canceled
	default:
	}

	body := struct{}{}
	id, _, err := doRequest[struct{}, api.AgentIdentification](a, timeout, http.MethodGet, "/id", &body)

	select {
	case <-a.unregistered.Recv():
		return context.Canceled
	default:
	}

	if err != nil {
		a.EnsureUnregistered()
		return err
	}

	if id.AgentID != a.id {
		a.EnsureUnregistered()
		return fmt.Errorf("Bad agent identification: expected %q but got %q", a.id, id.AgentID)
	}

	return nil
}

// Suspend signals to the Agent that it is not *currently* in use, sending a request to its /suspend
// endpoint
//
// If the Agent is unregistered before the call to Suspend() completes, the request will be
// cancelled and this method will return context.Canceled.
//
// If the request fails, the Agent will be unregistered.
func (a *Agent) Suspend(timeout time.Duration, handleError func(error)) {
	// Quick unregistered check:
	select {
	case <-a.unregistered.Recv():
		klog.Warningf(
			"Suspend called for Agent %s/%s that is already unregistered (probably *not* a race?)",
			a.serverAddr, a.id,
		)
		handleError(context.Canceled)
		return
	default:
	}

	body := api.SuspendAgent{ExpectedID: a.id}
	id, _, err := doRequest[api.SuspendAgent, api.AgentIdentification](
		a, timeout, http.MethodPost, "/suspend", &body,
	)

	select {
	case <-a.unregistered.Recv():
		handleError(context.Canceled)
		return
	default:
	}

	if err != nil {
		a.EnsureUnregistered()
		handleError(err)
		return
	}

	if id.AgentID != a.id {
		a.EnsureUnregistered()
		handleError(fmt.Errorf("Bad agent identification: expected %q but got %q", a.id, id.AgentID))
		return
	}

	a.suspended = false
}

// Resume attempts to restore the Agent as the current one in use, sending a request to its /resume
// endpoint
//
// If the Agent is unregistered before the call to Resume() completes, the request will be cancelled
// and this method will return context.Canceled.
//
// If the request fails, the Agent will be unregistered.
func (a *Agent) Resume(timeout time.Duration) error {
	// Quick unregistered check:
	select {
	case <-a.unregistered.Recv():
		klog.Warningf(
			"Resume called for Agent %s/%s that is already unregistered (probably *not* a race?)",
			a.serverAddr, a.id,
		)
		return context.Canceled
	default:
	}

	body := api.ResumeAgent{ExpectedID: a.id}
	id, _, err := doRequest[api.ResumeAgent, api.AgentIdentification](
		a, timeout, http.MethodPost, "/resume", &body,
	)

	select {
	case <-a.unregistered.Recv():
		return context.Canceled
	default:
	}

	if err != nil {
		a.EnsureUnregistered()
		return err
	}

	if id.AgentID != a.id {
		a.EnsureUnregistered()
		return fmt.Errorf("Bad agent identification: expected %q but got %q", a.id, id.AgentID)
	}

	return nil
}

// SpawnRequestUpscale requests that the Agent increase the resource allocation to this VM
//
// This method blocks until the request is picked up by the message queue, and returns without
// waiting for the request to complete (it'll do that on its own).
//
// The timeout applies only once the request is in-flight.
//
// This method MUST NOT be called while holding a.parent.lock; if that happens, it may deadlock.
func (a *Agent) SpawnRequestUpscale(timeout time.Duration, handleError func(error)) {
	// Quick unregistered check
	select {
	case <-a.unregistered.Recv():
		klog.Warningf(
			"RequestUpscale called for Agent %s/%s that is already unregistered (probably *not* a race?)",
			a.serverAddr, a.id,
		)
		handleError(context.Canceled)
		return
	default:
	}

	sendDone, recvDone := util.NewSingleSignalPair()

	go func() {
		// If we exit early, signal that we're done.
		defer sendDone.Send()

		unsetWantsUpscale := func() {
			// Unset s.wantsMemoryUpscale if the agent is still current. We want to allow further
			// requests to try again.
			a.parent.lock.Lock()
			defer a.parent.lock.Unlock()

			if a.parent.current == a {
				a.parent.wantsMemoryUpscale = false
			}
		}

		body := api.MoreResourcesRequest{
			MoreResources: api.MoreResources{Cpu: false, Memory: true},
			ExpectedID:    a.id,
		}
		// Pass the signal sender into doRequestWithStartSignal so that the signalling on
		// start-of-handling is done for us.
		id, _, err := doRequestWithStartSignal[api.MoreResourcesRequest, api.AgentIdentification](
			a, timeout, &sendDone, http.MethodPost, "/try-upscale", &body,
		)

		select {
		case <-a.unregistered.Recv():
			handleError(context.Canceled)
			return
		default:
		}

		if err != nil {
			unsetWantsUpscale()
			a.EnsureUnregistered()
			handleError(err)
			return
		}

		if id.AgentID != a.id {
			unsetWantsUpscale()
			a.EnsureUnregistered()
			handleError(fmt.Errorf("Bad agent identification: expected %q but got %q", a.id, id.AgentID))
			return
		}
	}()

	<-recvDone.Recv()
}
