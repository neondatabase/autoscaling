package agent

// The Dispatcher is our interface with the monitor. We interact via a websocket
// connection through a simple RPC-style protocol.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.uber.org/zap"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

const (
	MinMonitorProtocolVersion api.MonitorProtoVersion = api.MonitorProtoV1_0
	MaxMonitorProtocolVersion api.MonitorProtoVersion = api.MonitorProtoV1_0
)

// This struct represents the result of a dispatcher.Call. Because the SignalSender
// passed in can only be generic over one type - we have this mock enum. Only
// one field should ever be non-nil, and it should always be clear which field
// is readable. For example, the caller of dispatcher.call(HealthCheck { .. })
// should only read the healthcheck field.
type MonitorResult struct {
	Result       *api.DownscaleResult
	Confirmation *api.UpscaleConfirmation
	HealthCheck  *api.HealthCheck
}

// The Dispatcher is the main object managing the websocket connection to the
// monitor. For more information on the protocol, see pkg/api/types.go
type Dispatcher struct {
	// The underlying connection we are managing
	conn *websocket.Conn

	// When someone sends a message, the dispatcher will attach a transaction id
	// to it so that it knows when a response is back. When it receives a message
	// with the same transaction id, it knows that that is the response to the original
	// message and will send it down the SignalSender so the original sender can use it.
	waiters map[uint64]util.SignalSender[waiterResult]

	// lock guards mutating the waiters, exitError, and (closing) exitSignal field.
	// conn and lastTransactionID are all thread safe.
	// runner, exit, and protoVersion are never modified.
	lock sync.Mutex

	// The runner that this dispatcher is part of
	runner *Runner

	exit func(status websocket.StatusCode, err error, transformErr func(error) error)

	exitError  error
	exitSignal chan struct{}

	// lastTransactionID is the last transaction id. When we need a new one
	// we simply bump it and take the new number.
	//
	// In order to prevent collisions between the IDs generated here vs by
	// the monitor, we only generate even IDs, and the monitor only generates
	// odd ones. So generating a new value is done by adding 2.
	lastTransactionID atomic.Uint64

	protoVersion api.MonitorProtoVersion
}

type waiterResult struct {
	err error
	res *MonitorResult
}

// Create a new Dispatcher, establishing a connection with the vm-monitor and setting up all the
// background threads to manage the connection.
func NewDispatcher(
	ctx context.Context,
	logger *zap.Logger,
	addr string,
	runner *Runner,
	sendUpscaleRequested func(request api.MoreResources, withLock func()),
) (_finalDispatcher *Dispatcher, _ error) {
	// Create a new root-level context for this Dispatcher so that we can cancel if need be
	ctx, cancelRootContext := context.WithCancel(ctx)
	defer func() {
		// cancel on failure or panic
		if _finalDispatcher == nil {
			cancelRootContext()
		}
	}()

	connectTimeout := time.Second * time.Duration(runner.global.config.Monitor.ConnectionTimeoutSeconds)
	conn, protoVersion, err := connectToMonitor(ctx, logger, addr, connectTimeout)
	if err != nil {
		return nil, err
	}

	disp := &Dispatcher{
		conn:              conn,
		waiters:           make(map[uint64]util.SignalSender[waiterResult]),
		runner:            runner,
		lock:              sync.Mutex{},
		exit:              nil, // set below
		exitError:         nil,
		exitSignal:        make(chan struct{}),
		lastTransactionID: atomic.Uint64{}, // Note: initialized to 0, so it's even, as required.
		protoVersion:      *protoVersion,
	}
	disp.exit = func(status websocket.StatusCode, err error, transformErr func(error) error) {
		disp.lock.Lock()
		defer disp.lock.Unlock()

		if disp.Exited() {
			return
		}

		close(disp.exitSignal)
		disp.exitError = err
		cancelRootContext()

		var closeReason string
		if err != nil {
			if transformErr != nil {
				closeReason = transformErr(err).Error()
			} else {
				closeReason = err.Error()
			}
		} else {
			closeReason = "normal exit"
		}

		// Run the actual websocket closing in a separate goroutine so we don't block while holding
		// the lock. It can take up to 10s to close:
		//
		// > [Close] will write a WebSocket close frame with a timeout of 5s and then wait 5s for
		// > the peer to send a close frame.
		//
		// This *potentially* runs us into race issues, but those are probably less bad to deal
		// with, tbh.
		go disp.conn.Close(status, closeReason)
	}

	go func() {
		<-ctx.Done()
		disp.exit(websocket.StatusNormalClosure, nil, nil)
	}()

	msgHandlerLogger := logger.Named("message-handler")
	runner.spawnBackgroundWorker(ctx, msgHandlerLogger, "vm-monitor message handler", func(c context.Context, l *zap.Logger) {
		disp.run(c, l, sendUpscaleRequested)
	})
	runner.spawnBackgroundWorker(ctx, logger.Named("health-checks"), "vm-monitor health checks", func(ctx context.Context, logger *zap.Logger) {
		timeout := time.Second * time.Duration(runner.global.config.Monitor.ResponseTimeoutSeconds)
		// FIXME: make this duration configurable
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		// if we've had sequential failures for more than
		var firstSequentialFailure *time.Time
		continuedFailureAbortTimeout := time.Second * time.Duration(runner.global.config.Monitor.MaxHealthCheckSequentialFailuresSeconds)

		// if we don't have any errors, we will log only every 10th successful health check
		const logEveryNth = 10
		var okSequence int
		var failSequence int

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			startTime := time.Now()
			_, err := disp.Call(ctx, logger, timeout, "HealthCheck", api.HealthCheck{})
			endTime := time.Now()

			logFields := []zap.Field{
				zap.Duration("duration", endTime.Sub(startTime)),
			}
			if okSequence != 0 {
				logFields = append(logFields, zap.Int("okSequence", okSequence))
			}
			if failSequence != 0 {
				logFields = append(logFields, zap.Int("failSequence", failSequence))
			}

			if err != nil {
				// health check failed, reset the ok sequence count
				okSequence = 0
				failSequence++
				logger.Error("vm-monitor health check failed", append(logFields, zap.Error(err))...)

				if firstSequentialFailure == nil {
					now := time.Now()
					firstSequentialFailure = &now
				} else if since := time.Since(*firstSequentialFailure); since > continuedFailureAbortTimeout {
					err := fmt.Errorf("vm-monitor has been failing health checks for at least %s", continuedFailureAbortTimeout)
					logger.Error(fmt.Sprintf("%s, triggering connection restart", err.Error()))
					disp.exit(websocket.StatusInternalError, err, nil)
				}
			} else {
				// health check was successful, so reset the sequential failures count
				failSequence = 0
				okSequence++
				firstSequentialFailure = nil

				if okSequence%logEveryNth == 0 {
					logger.Info("vm-monitor health check successful", logFields...)
				}

				runner.status.update(runner.global, func(s podStatus) podStatus {
					now := time.Now()
					s.lastSuccessfulMonitorComm = &now
					return s
				})
			}
		}
	})
	return disp, nil
}

func connectToMonitor(
	ctx context.Context,
	logger *zap.Logger,
	addr string,
	timeout time.Duration,
) (_ *websocket.Conn, _ *api.MonitorProtoVersion, finalErr error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger.Info("Connecting to vm-monitor via websocket", zap.String("addr", addr))

	// We do not need to close the response body according to docs.
	// Doing so causes memory bugs.
	c, _, err := websocket.Dial(ctx, addr, nil) //nolint:bodyclose // see comment above
	if err != nil {
		return nil, nil, fmt.Errorf("error establishing websocket connection to %s: %w", addr, err)
	}

	// If we return early, make sure we close the websocket
	var failureReason websocket.StatusCode
	defer func() {
		if finalErr != nil {
			if failureReason == 0 {
				failureReason = websocket.StatusInternalError
			}
			c.Close(failureReason, finalErr.Error())
		}
	}()

	versionRange := api.VersionRange[api.MonitorProtoVersion]{
		Min: MinMonitorProtocolVersion,
		Max: MaxMonitorProtocolVersion,
	}
	logger.Info("Sending protocol version range", zap.Any("range", versionRange))

	// Figure out protocol version
	err = wsjson.Write(ctx, c, versionRange)
	if err != nil {
		return nil, nil, fmt.Errorf("error sending protocol range to monitor: %w", err)
	}

	logger.Info("Reading monitor version response")
	var resp api.MonitorProtocolResponse
	err = wsjson.Read(ctx, c, &resp)
	if err != nil {
		logger.Error("Failed to read monitor response", zap.Error(err))
		failureReason = websocket.StatusProtocolError
		return nil, nil, fmt.Errorf("Error reading vm-monitor response during protocol handshake: %w", err)
	}

	logger.Info("Got monitor version response", zap.Any("response", resp))
	if resp.Error != nil {
		logger.Error("Got error response from vm-monitor", zap.Any("response", resp), zap.String("error", *resp.Error))
		failureReason = websocket.StatusProtocolError
		return nil, nil, fmt.Errorf("Monitor returned error during protocol handshake: %q", *resp.Error)
	}

	logger.Info("negotiated protocol version with monitor", zap.Any("response", resp), zap.String("version", resp.Version.String()))
	return c, &resp.Version, nil
}

// ExitSignal returns a channel that is closed when the Dispatcher is no longer running
func (disp *Dispatcher) ExitSignal() <-chan struct{} {
	return disp.exitSignal
}

// Exited returns whether the Dispatcher is no longer running
//
// Exited will return true iff the channel returned by ExitSignal is closed.
func (disp *Dispatcher) Exited() bool {
	select {
	case <-disp.exitSignal:
		return true
	default:
		return false
	}
}

// ExitError returns the error that caused the dispatcher to exit, if there was one
func (disp *Dispatcher) ExitError() error {
	disp.lock.Lock()
	defer disp.lock.Unlock()
	return disp.exitError
}

// temporary method to hopefully help with https://github.com/neondatabase/autoscaling/issues/503
func (disp *Dispatcher) lenWaiters() int {
	disp.lock.Lock()
	defer disp.lock.Unlock()
	return len(disp.waiters)
}

// Send a message down the connection. Only call this method with types that
// SerializeMonitorMessage can handle.
func (disp *Dispatcher) send(ctx context.Context, logger *zap.Logger, id uint64, message any) error {
	data, err := api.SerializeMonitorMessage(message, id)
	if err != nil {
		return fmt.Errorf("error serializing message: %w", err)
	}
	// wsjson.Write serializes whatever is passed in, and go serializes []byte
	// by base64 encoding it, so use RawMessage to avoid serializing to []byte
	// (done by SerializeMonitorMessage), and then base64 encoding again
	raw := json.RawMessage(data)
	logger.Debug("sending message to monitor", zap.ByteString("message", raw))
	return wsjson.Write(ctx, disp.conn, &raw)
}

// registerWaiter registers a util.SignalSender to get notified when a
// message with the given id arrives.
func (disp *Dispatcher) registerWaiter(id uint64, sender util.SignalSender[waiterResult]) {
	disp.lock.Lock()
	defer disp.lock.Unlock()
	disp.waiters[id] = sender
}

// unregisterWaiter deletes a preexisting waiter without interacting with it.
func (disp *Dispatcher) unregisterWaiter(id uint64) {
	disp.lock.Lock()
	defer disp.lock.Unlock()
	delete(disp.waiters, id)
}

// Make a request to the monitor and wait for a response. The value passed as message must be a
// valid value to send to the monitor. See the docs for SerializeMonitorMessage for more.
//
// This function must NOT be called while holding disp.runner.lock.
func (disp *Dispatcher) Call(
	ctx context.Context,
	logger *zap.Logger,
	timeout time.Duration,
	messageType string,
	message any,
) (*MonitorResult, error) {
	id := disp.lastTransactionID.Add(2)
	sender, receiver := util.NewSingleSignalPair[waiterResult]()

	status := "internal error"
	defer func() {
		disp.runner.global.metrics.monitorRequestsOutbound.WithLabelValues(messageType, status).Inc()
	}()

	// register the waiter *before* sending, so that we avoid a potential race where we'd get a
	// reply to the message before being ready to receive it.
	disp.registerWaiter(id, sender)
	err := disp.send(ctx, logger, id, message)
	if err != nil {
		logger.Error("failed to send message", zap.Any("message", message), zap.Error(err))
		disp.unregisterWaiter(id)
		status = "[error: failed to send]"
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-receiver.Recv():
		if result.err != nil {
			status = fmt.Sprintf("[error: %s]", result.err)
			return nil, errors.New("monitor experienced an internal error")
		}

		status = "ok"
		return result.res, nil
	case <-timer.C:
		err := fmt.Errorf("timed out waiting %v for monitor response", timeout)
		disp.unregisterWaiter(id)
		status = "[error: timed out waiting for response]"
		return nil, err
	}
}

func extractField[T any](data map[string]interface{}, key string) (*T, error) {
	field, ok := data[key]
	if !ok {
		return nil, fmt.Errorf("data had no key %q", key)
	}

	coerced, ok := field.(T)
	if !ok {
		return nil, fmt.Errorf("data[%q] was not of type %T", key, *new(T))
	}

	return &coerced, nil
}

type messageHandlerFuncs struct {
	handleUpscaleRequest      func(api.UpscaleRequest)
	handleUpscaleConfirmation func(api.UpscaleConfirmation, uint64) error
	handleDownscaleResult     func(api.DownscaleResult, uint64) error
	handleMonitorError        func(api.InternalError, uint64) error
	handleHealthCheck         func(api.HealthCheck, uint64) error
}

// Handle messages from the monitor. Make sure that all message types the monitor
// can send are included in the inner switch statement.
func (disp *Dispatcher) HandleMessage(
	ctx context.Context,
	logger *zap.Logger,
	handlers messageHandlerFuncs,
) error {
	// Deserialization has several steps:
	// 1. Deserialize into an unstructured map[string]interface{}
	// 2. Read the `type` field to know the type of the message
	// 3. Then try to to deserialize again, but into that specific type
	// 4. All message also come with an integer id under the key `id`

	// wsjson.Read tries to deserialize the message. If we were to read to a
	// []byte, it would base64 encode it as part of deserialization. json.RawMessage
	// avoids this, and we manually deserialize later
	var message json.RawMessage
	if err := wsjson.Read(ctx, disp.conn, &message); err != nil {
		return fmt.Errorf("Error receiving message: %w", err)
	}

	logger.Debug("(pre-decoding): received a message", zap.ByteString("message", message))

	var unstructured map[string]interface{}
	if err := json.Unmarshal(message, &unstructured); err != nil {
		return fmt.Errorf("Error deserializing message: %q", string(message))
	}

	typeStr, err := extractField[string](unstructured, "type")
	if err != nil {
		return fmt.Errorf("Error extracting 'type' field: %w", err)
	}

	// go thinks all json numbers are float64 so we first deserialize to that to
	// avoid the type error, then cast to uint64
	f, err := extractField[float64](unstructured, "id")
	if err != nil {
		return fmt.Errorf("Error extracting 'id field: %w", err)
	}
	id := uint64(*f)

	var rootErr error

	// now that we have the waiter's ID, make sure that if there's some failure past this point, we
	// propagate that along to the monitor and remove it
	defer func() {
		// speculatively determine the root error, to send that along to the instance of Call
		// waiting for it.
		var err error

		panicPayload := recover()
		if panicPayload != nil {
			err = errors.New("panicked")
		} else if rootErr != nil {
			err = rootErr
		} else {
			// if HandleMessage bailed without panicking or setting rootErr, but *also* without
			// sending a message to the waiter, we should make sure that *something* gets sent, so
			// the message doesn't just time out. But we don't have more information, so the error
			// is still just "unknown".
			err = errors.New("unknown")
		}

		disp.lock.Lock()
		defer disp.lock.Unlock()
		if sender, ok := disp.waiters[id]; ok {
			sender.Send(waiterResult{err: err, res: nil})
			delete(disp.waiters, id)
		} else if rootErr != nil {
			// we had some error while handling the message with this ID, and there wasn't a
			// corresponding waiter. We should make note of this in the metrics:
			status := fmt.Sprintf("[error: %s]", rootErr)
			disp.runner.global.metrics.monitorRequestsInbound.WithLabelValues(*typeStr, status)
		}

		// resume panicking if we were before
		if panicPayload != nil {
			panic(panicPayload)
		}
	}()

	// Helper function to handle common unmarshalling logic
	unmarshal := func(value any) error {
		if err := json.Unmarshal(message, value); err != nil {
			rootErr = errors.New("Failed unmarshaling JSON")
			err := fmt.Errorf("Error unmarshaling %s: %w", *typeStr, err)
			logger.Error(rootErr.Error(), zap.Error(err))
			// we're already on the error path anyways
			_ = disp.send(ctx, logger, id, api.InvalidMessage{Error: err.Error()})
			return err
		}

		return nil
	}

	switch *typeStr {
	case "UpscaleRequest":
		var req api.UpscaleRequest
		if err := unmarshal(&req); err != nil {
			return err
		}
		handlers.handleUpscaleRequest(req)
		return nil
	case "UpscaleConfirmation":
		var confirmation api.UpscaleConfirmation
		if err := unmarshal(&confirmation); err != nil {
			return err
		}
		return handlers.handleUpscaleConfirmation(confirmation, id)
	case "DownscaleResult":
		var res api.DownscaleResult
		if err := unmarshal(&res); err != nil {
			return err
		}
		return handlers.handleDownscaleResult(res, id)
	case "InternalError":
		var monitorErr api.InternalError
		if err := unmarshal(&monitorErr); err != nil {
			return err
		}
		return handlers.handleMonitorError(monitorErr, id)
	case "HealthCheck":
		var healthCheck api.HealthCheck
		if err := unmarshal(&healthCheck); err != nil {
			return err
		}
		return handlers.handleHealthCheck(healthCheck, id)
	case "InvalidMessage":
		var warning api.InvalidMessage
		if err := unmarshal(&warning); err != nil {
			return err
		}
		logger.Warn("Received notification we sent an invalid message", zap.Any("warning", warning))
		return nil
	default:
		rootErr = errors.New("Received unknown message type")
		return disp.send(
			ctx,
			logger,
			id,
			api.InvalidMessage{Error: fmt.Sprintf("Received message of unknown type: %q", *typeStr)},
		)
	}
}

// Long running function that orchestrates all requests/responses.
func (disp *Dispatcher) run(ctx context.Context, logger *zap.Logger, upscaleRequester func(_ api.MoreResources, withLock func())) {
	logger.Info("Starting message handler")

	// Utility for logging + returning an error when we get a message with an
	// id we're unaware of. Note: unknownMessage is not a message type.
	handleUnkownMessage := func(messageType string, id uint64) error {
		fmtString := "Received %s with id %d but id is unknown or already timed out waiting for a reply"
		msg := fmt.Sprintf(fmtString, messageType, id)
		logger.Warn(msg, zap.Uint64("id", id))
		return disp.send(ctx, logger, id, api.InvalidMessage{Error: msg})
	}

	// Does not take a message id because we don't know when the agent will
	// upscale. The monitor will get the result back as a NotifyUpscale message
	// from us, with a new id.
	handleUpscaleRequest := func(req api.UpscaleRequest) {
		// TODO: it shouldn't be this function's responsibility to update metrics.
		defer func() {
			disp.runner.global.metrics.monitorRequestsInbound.WithLabelValues("UpscaleRequest", "ok").Inc()
		}()

		resourceReq := api.MoreResources{
			Cpu:    false,
			Memory: true,
		}

		upscaleRequester(resourceReq, func() {
			logger.Info("Updating requested upscale", zap.Any("requested", resourceReq))
		})
	}
	handleUpscaleConfirmation := func(_ api.UpscaleConfirmation, id uint64) error {
		disp.lock.Lock()
		defer disp.lock.Unlock()

		sender, ok := disp.waiters[id]
		if ok {
			logger.Info("vm-monitor confirmed upscale", zap.Uint64("id", id))
			sender.Send(waiterResult{
				err: nil,
				res: &MonitorResult{
					Confirmation: &api.UpscaleConfirmation{},
					Result:       nil,
					HealthCheck:  nil,
				},
			})
			// Don't forget to delete the waiter
			delete(disp.waiters, id)
			return nil
		} else {
			return handleUnkownMessage("UpscaleConfirmation", id)
		}
	}
	handleDownscaleResult := func(res api.DownscaleResult, id uint64) error {
		disp.lock.Lock()
		defer disp.lock.Unlock()

		sender, ok := disp.waiters[id]
		if ok {
			logger.Info("vm-monitor returned downscale result", zap.Uint64("id", id), zap.Any("result", res))
			sender.Send(waiterResult{
				err: nil,
				res: &MonitorResult{
					Result:       &res,
					Confirmation: nil,
					HealthCheck:  nil,
				},
			})
			// Don't forget to delete the waiter
			delete(disp.waiters, id)
			return nil
		} else {
			return handleUnkownMessage("DownscaleResult", id)
		}
	}
	handleMonitorError := func(err api.InternalError, id uint64) error {
		disp.lock.Lock()
		defer disp.lock.Unlock()

		sender, ok := disp.waiters[id]
		if ok {
			logger.Warn(
				"vm-monitor experienced an internal error",
				zap.Uint64("id", id),
				zap.String("error", err.Error),
			)
			// Indicate to the receiver that an error occurred
			sender.Send(waiterResult{
				err: errors.New("vm-monitor internal error"),
				res: nil,
			})
			// Don't forget to delete the waiter
			delete(disp.waiters, id)
			return nil
		} else {
			return handleUnkownMessage("MonitorError", id)
		}
	}
	handleHealthCheck := func(confirmation api.HealthCheck, id uint64) error {
		disp.lock.Lock()
		defer disp.lock.Unlock()

		sender, ok := disp.waiters[id]
		if ok {
			logger.Debug("vm-monitor responded to health check", zap.Uint64("id", id))
			// Indicate to the receiver that an error occurred
			sender.Send(waiterResult{
				err: nil,
				res: &MonitorResult{
					HealthCheck:  &api.HealthCheck{},
					Result:       nil,
					Confirmation: nil,
				},
			})
			// Don't forget to delete the waiter
			delete(disp.waiters, id)
			return nil
		} else {
			return handleUnkownMessage("HealthCheck", id)
		}
	}

	handlers := messageHandlerFuncs{
		handleUpscaleRequest:      handleUpscaleRequest,
		handleUpscaleConfirmation: handleUpscaleConfirmation,
		handleDownscaleResult:     handleDownscaleResult,
		handleMonitorError:        handleMonitorError,
		handleHealthCheck:         handleHealthCheck,
	}

	for {
		err := disp.HandleMessage(ctx, logger, handlers)
		if err != nil {
			if ctx.Err() != nil {
				// The context is already cancelled, so this error is mostly likely
				// expected. For example, if the context is cancelled because the
				// runner exited, we should expect to fail to read off the connection,
				// which is closed by the server exit.
				logger.Warn("Error handling message", zap.Error(err))
			} else {
				logger.Error("Error handling message, shutting down connection", zap.Error(err))
				err = fmt.Errorf("Error handling message: %w", err)
				// note: in theory we *could* be more descriptive with these statuses, but the only
				// consumer of this API is the vm-monitor, and it doesn't check those.
				//
				// Also note: there's a limit on the size of the close frame we're allowed to send,
				// so the actual error we use to exit with must be somewhat reduced in size. These
				// "Error handling message" errors can get quite long, so we'll only use the root
				// cause of the error for the message.
				disp.exit(websocket.StatusInternalError, err, util.RootError)
			}
			return
		}
	}
}
