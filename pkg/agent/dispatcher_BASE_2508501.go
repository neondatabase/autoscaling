package informant

// The Dispatcher is our interface with the monitor. We interact via a websocket
// connection through a simple RPC-style protocol.

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type MonitorResult struct {
	Result       *api.DownscaleResult
	Confirmation struct{}
}

// The Dispatcher is the main object managing the websocket connection to the
// monitor. For more information on the protocol, see Rust docs and transport.go
type Dispatcher struct {
	// The underlying connection we are managing
	conn *websocket.Conn

	// When someone sends a message, the dispatcher will attach a transaction id
	// to it so that it knows when a response is back. When it receives a message
	// with the same transaction id, it knows that that is the repsonse to the original
	// message and will send it down the SignalSender so the original sender can use it.
	waiters map[uint64]util.SignalSender[*MonitorResult]

	// A message is sent along this channel when an upscale is requested.
	// When the informant.NewState is called, a goroutine will be spawned that
	// just tries to receive off the channel and then request the upscale.
	// A different way to do this would be to keep a backpointer to the parent
	// `State` and then just call a method on it when an upscale is requested.
	notifier util.CondChannelSender

	// This counter represents the current transaction id. When we need a new one
	// we simply bump it and take the new number.
	counter uint64

	logger *zap.Logger

	protoVersion api.MonitorProtoVersion
}

// Create a new Dispatcher, establishing a connection with the informant.
// Note that this does not immediately start the Dispatcher. Call Run() to start it.
func NewDispatcher(logger *zap.Logger, addr string, notifier util.CondChannelSender) (disp Dispatcher, _ error) {
	ctx := context.TODO()

	logger.Info("connecting via websocket", zap.String("addr", addr))

	// We do not need to close the response body according to docs.
	// Doing so causes memory bugs.
	c, _, err := websocket.Dial(ctx, addr, nil) //nolint:bodyclose // see comment above
	if err != nil {
		return disp, fmt.Errorf("error creating dispatcher: %w", err)
	}

	// Figure out protocol version
	err = wsjson.Write(
		ctx,
		c,
		api.VersionRange[api.MonitorProtoVersion]{
			Min: api.MonitorProtoV1_0,
			Max: api.MonitorProtoV1_0,
		},
	)
	if err != nil {
		return Dispatcher{}, fmt.Errorf("error sending protocol range to monitor: %w", err)
	}
	var version api.MonitorProtocolResponse
	err = wsjson.Read(ctx, c, &version)
	if err != nil {
		return Dispatcher{}, fmt.Errorf("error reading monitor response during protocol handshake: %w", err)
	}
	if version.Error != nil {
		return Dispatcher{}, fmt.Errorf("monitor returned error during protocol handshake: %s", *version.Error)
	}
	logger.Info("negotiated protocol with monitor", zap.String("protocol", version.Version.String()))

	disp = Dispatcher{
		conn:         c,
		notifier:     notifier,
		waiters:      make(map[uint64]util.SignalSender[*MonitorResult]),
		counter:      0,
		logger:       logger.Named("dispatcher"),
		protoVersion: version.Version,
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
	return wsjson.Write(ctx, disp.conn, &raw)
}

// Make a request to the monitor. The dispatcher will handle returning a response
// on the provided SignalSender. The value passed into message must be a valid value
// to send to the monitor. See the docs for SerializeInformantMessage.
func (disp *Dispatcher) Call(ctx context.Context, sender util.SignalSender[*MonitorResult], message any) error {
	id := disp.counter
	disp.counter += 1
	err := disp.send(ctx, id, message)
	if err != nil {
		disp.logger.Error("failed to send message", zap.Any("message", message), zap.Error(err))
	}
	disp.waiters[id] = sender
	return nil
}

// Handle messages from the monitor. Make sure that all message types the monitor
// can send are included in the inner switch statement.
func (disp *Dispatcher) HandleMessage(
	ctx context.Context,
	logger *zap.Logger,
	handleUpscaleRequest func(api.UpscaleRequest),
	handleUpscaleConfirmation func(api.UpscaleConfirmation, uint64) error,
	handleDownscaleResult func(api.DownscaleResult, uint64) error,
	handleMonitorError func(api.InternalError, uint64) error,
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

	var unstructured map[string]interface{}
	if err := json.Unmarshal(message, &unstructured); err != nil {
		return fmt.Errorf("error deserializing message: \"%s\"", string(message))
	}

	typeField, ok := unstructured["type"]
	if !ok {
		return fmt.Errorf("message did not have \"type\" field")
	}
	typeStr, ok := typeField.(string)
	if !ok {
		return fmt.Errorf("value <%s> with key \"type\" was not a string", typeField)
	}

	idField, ok := unstructured["id"]
	if !ok {
		return fmt.Errorf("message did not have \"id\" field")
	}

	// Go expects JSON numbers to be float64, so we first assert to that then
	// convert to uint64. Trying to go straight to (uint64) will fail
	f, ok := idField.(float64)
	if !ok {
		return fmt.Errorf("value <%s> with key \"id\" was not a number", idField)
	}
	id := uint64(f)

	switch typeStr {
	case "UpscaleRequest":
		var req api.UpscaleRequest
		if err := json.Unmarshal(message, &req); err != nil {
			return fmt.Errorf("error unmarshaling UpscaleRequest: %w", err)
		}
		handleUpscaleRequest(req)
		return nil
	case "UpscaleConfirmation":
		var confirmation api.UpscaleConfirmation
		if err := json.Unmarshal(message, &confirmation); err != nil {
			return fmt.Errorf("error unmarshaling UpscaleConfirmation: %w", err)
		}
		return handleUpscaleConfirmation(confirmation, id)
	case "DownscaleResult":
		var res api.DownscaleResult
		if err := json.Unmarshal(message, &res); err != nil {
			return fmt.Errorf("error unmarshaling DownscaleResult: %w", err)
		}
		return handleDownscaleResult(res, id)
	case "InternalError":
		var monitorErr api.InternalError
		if err := json.Unmarshal(message, &monitorErr); err != nil {
			return fmt.Errorf("error unmarshaling InternalError: %w", err)
		}
		return handleMonitorError(monitorErr, id)
	default:
		return disp.send(
			ctx,
			id,
			api.InvalidMessage{Error: fmt.Sprintf("received message of unknown type: <%s>", typeStr)},
		)
	}
}

// Long running function that performs all orchestrates all requests/responses.
func (disp *Dispatcher) run() {
	disp.logger.Info("Starting.")
	ctx := context.Background()
	for {
		logger := disp.logger.Named("message-handler")
		// Does not take a message id because we don't know when the agent will
		// upscale. The monitor will get the result back as a NotifyUpscale message
		// from us, with a new id.
		handleUpscaleRequest := func(api.UpscaleRequest) {
			disp.notifier.Send()
		}
		handleUpscaleConfirmation := func(_ api.UpscaleConfirmation, id uint64) error {
			sender, ok := disp.waiters[id]
			if ok {
				sender.Send(&MonitorResult{Result: nil, Confirmation: struct{}{}})
				// Don't forget to delete the waiter
				delete(disp.waiters, id)
				return nil
			} else {
				fmtString := "received UpscaleConfirmation with id %d but no record of previous message with that id"
				return disp.send(ctx, id, api.InvalidMessage{Error: fmt.Sprintf(fmtString, id)})
			}
		}
		handleDownscaleResult := func(res api.DownscaleResult, id uint64) error {
			sender, ok := disp.waiters[id]
			if ok {
				sender.Send(&MonitorResult{Result: &res, Confirmation: struct{}{}})
				// Don't forget to delete the waiter
				delete(disp.waiters, id)
				return nil
			} else {
				fmtString := "received DownscaleResult with id %d but no record of previous message with that id"
				return disp.send(ctx, id, api.InvalidMessage{Error: fmt.Sprintf(fmtString, id)})
			}
		}
		handleMonitorError := func(err api.InternalError, id uint64) error {
			sender, ok := disp.waiters[id]
			if ok {
				logger.Warn("monitor experienced an internal error", zap.String("error", err.Error))
				// Indicate to the receiver that an error occured
				sender.Send(nil)
				// Don't forget to delete the waiter
				delete(disp.waiters, id)
				return nil
			} else {
				fmtString := "received InternalError with id %d but no record of previous message with that id"
				return disp.send(ctx, id, api.InvalidMessage{Error: fmt.Sprintf(fmtString, id)})
			}
		}

		err := disp.HandleMessage(
			ctx,
			logger,
			handleUpscaleRequest,
			handleUpscaleConfirmation,
			handleDownscaleResult,
			handleMonitorError,
		)
		if err != nil {
			logger.Error("error handling message -> panicking", zap.Error(err))
			// We actually want to panic here so we get respawned by the inittab,
			// and so the monitor's connection is closed and it also gets restarted
			panic(err)
		}
	}
}
