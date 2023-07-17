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
	// to it so that it knows when a response is back. When it receives a packet
	// with the same transaction id, it knows that that is the repsonse to the original
	// message and will send it down the oneshot so the original sender can use it.
	waiters map[uint64]util.SignalSender[MonitorResult]

	// A message is sent along this channel when an upscale is requested.
	// When the informant.NewState is called, a goroutine will be spawned that
	// just tries to receive off the channel and then request the upscale.
	// A different way to do this would be to keep a backpointer to the parent
	// `State` and then just call a method on it when an upscale is requested.
	notifier util.CondChannelSender

	// This counter represents the current transaction id. When we need a new one
	// we simply bump it and take the new number.
	//
	// Only we care about this number. The other side will just send back packets
	// with the id of the request, but they never do anything with the number.
	counter uint64

	logger *zap.Logger
}

// Create a new Dispatcher, establishing a connection with the informant.
// Note that this does not immediately start the Dispatcher. Call Run() to start it.
func NewDispatcher(logger *zap.Logger, addr string, notifier util.CondChannelSender) (disp Dispatcher, _ error) {
	// FIXME: have this context actually do something? As of now it's just being
	// passed around to satisfy typing. Maybe it's not really necessary.
	ctx := context.Background()

	logger.Info("Connecting via websocket.", zap.String("addr", addr))

	// We do not need to close the response body according to docs.
	// Doing so causes memory bugs.
	c, _, err := websocket.Dial(ctx, addr, nil) //nolint:bodyclose // see comment above
	if err != nil {
		return disp, fmt.Errorf("Error creating dispatcher: %w", err)
	}

	disp = Dispatcher{
		conn:     c,
		notifier: notifier,
		waiters:  make(map[uint64]util.SignalSender[MonitorResult]),
		counter:  0,
		logger:   logger.Named("dispatcher"),
	}
	return disp, nil
}

// Send a packet down the connection.
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
// on the provided oneshot.
//
// *Note*: sending a RequestUpscale to the monitor is incorrect. The monitor does
// not (and should) not know how to handle this and will panic. Likewise, we panic
// upon receiving a TryDownscale or NotifyUpscale request.
func (disp *Dispatcher) Call(ctx context.Context, sender util.SignalSender[MonitorResult], message any) error {
	id := disp.counter
	disp.counter += 1
	err := disp.send(ctx, id, message)
	if err != nil {
		disp.logger.Error("failed to send packet", zap.Any("message", message), zap.Error(err))
	}
	disp.waiters[id] = sender
	return nil
}

func (disp *Dispatcher) HandlePacket(
	ctx context.Context,
	logger *zap.Logger,
	handleUpscaleRequest func(api.UpscaleRequest),
	handleUpscaleConfirmation func(api.UpscaleConfirmation, uint64) error,
	handleDownscaleResult func(api.DownscaleResult, uint64) error,
) error {
	// wsjson.Read tries to deserialize the message. If we were to read to a
	// []byte, it would base64 encode it as part of deserialization. json.RawMessage
	// avoids this, and we manually deserialize later
	var message json.RawMessage
	if err := wsjson.Read(ctx, disp.conn, &message); err != nil {
		return fmt.Errorf("error receiving message: %w", err)
	}

	var unstructured map[string]interface{}
	if err := json.Unmarshal(message, &unstructured); err != nil {
		panic(err)
	}

	typeField, ok := unstructured["type"]
	if !ok {
		return fmt.Errorf("packet did not have \"type\" field")
	}
	typeStr, ok := typeField.(string)
	if !ok {
		return fmt.Errorf("value <%s> with key \"type\" was not a string", typeField)
	}

	idField, ok := unstructured["id"]
	if !ok {
		return fmt.Errorf("packet did not have \"id\" field")
	}

	// Go expects JSON numbers to be float64, so we first assert to that then
	// convert to uint64
	f, ok := idField.(float64)
	if !ok {
		return fmt.Errorf("value <%s> with key \"id\" was not a number", idField)
	}
	id := uint64(f)

	switch typeStr {
	case "UpscaleRequest":
		{
			var req api.UpscaleRequest
			if err := json.Unmarshal(message, &req); err != nil {
				return fmt.Errorf("error unmarshaling UpscaleRequest: %w", err)
			}
			handleUpscaleRequest(req)
			return nil
		}
	case "UpscaleConfirmation":
		{
			var confirmation api.UpscaleConfirmation
			if err := json.Unmarshal(message, &confirmation); err != nil {
				return fmt.Errorf("error unmarshaling UpscaleConfirmation: %w", err)
			}
			return handleUpscaleConfirmation(confirmation, id)
		}
	case "DownscaleResult":
		{
			var res api.DownscaleResult
			if err := json.Unmarshal(message, &res); err != nil {
				return fmt.Errorf("error unmarshaling DownscaleResult: %w", err)
			}
			return handleDownscaleResult(res, id)

		}
	default:
		{
			err := disp.send(
				ctx,
				id,
				api.InvalidMessage{Error: fmt.Sprintf("received packet of unknown type: <%s>", typeStr)},
			)
			if err != nil {
				return err
			}
			return nil
		}
	}
}

// Long running function that performs all orchestrates all requests/responses.
func (disp *Dispatcher) run() {
	disp.logger.Info("Starting.")
	ctx := context.Background()
	for {
		logger := disp.logger.Named("message-handler")
		handleUpscaleRequest := func(api.UpscaleRequest) {
			disp.notifier.Send()
		}
		handleUpscaleConfirmation := func(_ api.UpscaleConfirmation, id uint64) error {
			sender, ok := disp.waiters[id]
			if ok {
				sender.Send(MonitorResult{Result: nil, Confirmation: struct{}{}})
				// Don't forget to delete the waiter
				delete(disp.waiters, id)
			} else {
				fmtString := "received UpscaleConfirmation with id %d but no record of previous packet with that id"
				err := disp.send(ctx, id, api.InvalidMessage{Error: fmt.Sprintf(fmtString, id)})
				if err != nil {
					return err
				}
			}
			return nil
		}
		handleDownscaleResult := func(res api.DownscaleResult, id uint64) error {
			sender, ok := disp.waiters[id]
			if ok {
				sender.Send(MonitorResult{Result: &res, Confirmation: struct{}{}})
				// Don't forget to delete the waiter
				delete(disp.waiters, id)
			} else {
				fmtString := "received DownscaleResult with id %d but no record of previous packet with that id"
				err := disp.send(ctx, id, api.InvalidMessage{Error: fmt.Sprintf(fmtString, id)})
				if err != nil {
					return err
				}
			}
			return nil
		}

		err := disp.HandlePacket(
			ctx,
			logger,
			handleUpscaleRequest,
			handleUpscaleConfirmation,
			handleDownscaleResult,
		)
		if err != nil {
			logger.Error("error handling message", zap.Error(err))
		}
	}
}
