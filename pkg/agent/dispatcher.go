package agent

// The Dispatcher is our interface with the monitor. We interact via a websocket
// connection through a simple RPC-style protocol.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

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
	// with the same transaction id, it knows that that is the repsonse to the original
	// message and will send it down the SignalSender so the original sender can use it.
	waiters map[uint64]util.SignalSender[*MonitorResult]

	// lock guards mutating the waiters field. conn, logger, and nextTransactionID
	// are all thread safe. server and protoVersion are never modified.
	lock sync.Mutex

	// The InformantServer that this dispatcher is part of
	server *InformantServer

	// lastTransactionID is the last transaction id. When we need a new one
	// we simply bump it and take the new number.
	lastTransactionID atomic.Uint64

	logger *zap.Logger

	protoVersion api.MonitorProtoVersion
}

// Create a new Dispatcher, establishing a connection with the informant.
// Note that this does not immediately start the Dispatcher. Call Run() to start it.
func NewDispatcher(logger *zap.Logger, addr string, parent *InformantServer) (disp *Dispatcher, _ error) {
	ctx := context.TODO()

	logger.Info("connecting via websocket", zap.String("addr", addr))

	// We do not need to close the response body according to docs.
	// Doing so causes memory bugs.
	c, _, err := websocket.Dial(ctx, addr, nil) //nolint:bodyclose // see comment above
	if err != nil {
		return disp, fmt.Errorf("error establishing websocket connection to %s: %w", addr, err)
	}

	// Figure out protocol version
	err = wsjson.Write(
		ctx,
		c,
		api.VersionRange[api.MonitorProtoVersion]{
			Min: MinMonitorProtocolVersion,
			Max: MaxMonitorProtocolVersion,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error sending protocol range to monitor: %w", err)
	}
	var version api.MonitorProtocolResponse
	err = wsjson.Read(ctx, c, &version)
	if err != nil {
		return nil, fmt.Errorf("error reading monitor response during protocol handshake: %w", err)
	}
	if version.Error != nil {
		return nil, fmt.Errorf("monitor returned error during protocol handshake: %q", *version.Error)
	}
	logger.Info("negotiated protocol version with monitor", zap.String("version", version.Version.String()))

	disp = &Dispatcher{
		conn:              c,
		waiters:           make(map[uint64]util.SignalSender[*MonitorResult]),
		lastTransactionID: atomic.Uint64{},
		logger:            logger.Named("dispatcher"),
		protoVersion:      version.Version,
		server:            parent,
		lock:              sync.Mutex{},
	}
	return disp, nil
}

// Send a message down the connection. Only call this method with types that
// SerializeInformantMessage can handle.
func (disp *Dispatcher) send(ctx context.Context, id uint64, message any) error {
	data, err := api.SerializeInformantMessage(message, id)
	if err != nil {
		return fmt.Errorf("error serializing message: %w", err)
	}
	// wsjson.Write serializes whatever is passed in, and go serializes []byte
	// by base64 encoding it, so use RawMessage to avoid serializing to []byte
	// (done by SerializeInformantMessage), and then base64 encoding again
	raw := json.RawMessage(data)
	disp.logger.Info("sending message to monitor", zap.ByteString("message", raw))
	return wsjson.Write(ctx, disp.conn, &raw)
}

// registerWaiter registers a util.SignalSender to get notified when a
// message with the given id arrives.
func (disp *Dispatcher) registerWaiter(id uint64, sender util.SignalSender[*MonitorResult]) {
	disp.lock.Lock()
	defer disp.lock.Unlock()
	disp.waiters[id] = sender
}

// Make a request to the monitor. The dispatcher will handle returning a response
// on the provided SignalSender. The value passed into message must be a valid value
// to send to the monitor. See the docs for SerializeInformantMessage.
func (disp *Dispatcher) Call(ctx context.Context, sender util.SignalSender[*MonitorResult], message any) error {
	id := disp.lastTransactionID.Add(1)
	err := disp.send(ctx, id, message)
	if err != nil {
		disp.logger.Error("failed to send message", zap.Any("message", message), zap.Error(err))
	}
	disp.registerWaiter(id, sender)
	return nil
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
		return fmt.Errorf("error receiving message: %w", err)
	}
	logger.Info("(pre-decoding): received a message", zap.ByteString("message", message))

	var unstructured map[string]interface{}
	if err := json.Unmarshal(message, &unstructured); err != nil {
		return fmt.Errorf("error deserializing message: %q", string(message))
	}

	typeStr, err := extractField[string](unstructured, "type")
	if err != nil {
		return fmt.Errorf("error extracting 'type' field: %w", err)
	}

	// go thinks all json numbers are float64 so we first deserialize to that to
	// avoid the type error, then cast to uint64
	f, err := extractField[float64](unstructured, "id")
	if err != nil {
		return fmt.Errorf("error extracting 'id field: %w", err)
	}
	id := uint64(*f)

	// Helper function to handle common unmarshalling logic
	unmarshal := func(value any) error {
		if err := json.Unmarshal(message, value); err != nil {
			err := fmt.Errorf("error unmarshaling %s: %w", *typeStr, err)
			disp.send(ctx, id, api.InvalidMessage{Error: err.Error()})
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
		disp.logger.Warn("received notification we sent an invalid message", zap.Any("warning", warning))
		return nil
	default:
		return disp.send(
			ctx,
			id,
			api.InvalidMessage{Error: fmt.Sprintf("received message of unknown type: %q", *typeStr)},
		)
	}
}

// Long running function that orchestrates all requests/responses.
func (disp *Dispatcher) run() {
	ctx := context.Background()
	logger := disp.logger.Named("message-handler")
	logger.Info("starting message handler")

	// Utility for logging + returning an error when we get a message with an
	// id we're unaware of. Note: unknownMessage is not a message type.
	handleUnkownMessage := func(messageType string, id uint64) error {
		fmtString := "received %s with id %d but no record of previous message with that id"
		msg := fmt.Sprintf(fmtString, messageType, id)
		logger.Warn(msg, zap.Uint64("id", id))
		return disp.send(ctx, id, api.InvalidMessage{Error: msg})
	}

	// Does not take a message id because we don't know when the agent will
	// upscale. The monitor will get the result back as a NotifyUpscale message
	// from us, with a new id.
	handleUpscaleRequest := func(req api.UpscaleRequest) {
		disp.server.runner.lock.Lock()
		defer disp.server.runner.lock.Unlock()

		disp.server.upscaleRequested.Send()

		resourceReq := api.MoreResources{
			Cpu:    false,
			Memory: true,
		}

		logger.Info(
			"Updating requested upscale",
			zap.Any("oldRequested", disp.server.runner.requestedUpscale),
			zap.Any("newRequested", resourceReq),
		)
		disp.server.runner.requestedUpscale = resourceReq
	}
	handleUpscaleConfirmation := func(_ api.UpscaleConfirmation, id uint64) error {
		disp.lock.Lock()
		defer disp.lock.Unlock()

		sender, ok := disp.waiters[id]
		if ok {
			logger.Info("monitor confirmed upscale", zap.Uint64("id", id))
			sender.Send(&MonitorResult{
				Confirmation: &api.UpscaleConfirmation{},
				Result:       nil,
				HealthCheck:  nil,
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
			logger.Info("monitor returned downscale result", zap.Uint64("id", id))
			sender.Send(&MonitorResult{
				Result:       &res,
				Confirmation: nil,
				HealthCheck:  nil,
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
				"monitor experienced an internal error",
				zap.String("error", err.Error),
				zap.Uint64("id", id),
			)
			// Indicate to the receiver that an error occured
			sender.Send(nil)
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
			logger.Info("monitor responded to health check", zap.Uint64("id", id))
			// Indicate to the receiver that an error occured
			sender.Send(&MonitorResult{
				HealthCheck:  &api.HealthCheck{},
				Result:       nil,
				Confirmation: nil,
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
		err := disp.HandleMessage(
			ctx,
			logger,
			handlers,
		)
		if err != nil {
			logger.Error("error handling message -> panicking", zap.Error(err))
			// TODO: fix this comment for the agent
			// We actually want to panic here so we get respawned by the inittab,
			// and so the monitor's connection is closed and it also gets restarted
			panic(err)
		}
	}
}
