package informant

// The Dispatcher is our interface with the monitor. We interact via a websocket
// connection through a simple RPC-style protocol.

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type MonitorResult struct {
	Result       *DownscaleResult
	Confirmation struct{}
}

// The Dispatcher is the main object managing the websocket connection to the
// monitor. For more information on the protocol, see Rust docs and transport.go
type Dispatcher struct {
	// The underlying connection we are managing
	Conn *websocket.Conn

	// When someone sends a message, the dispatcher will attach a transaction id
	// to it so that it knows when a response is back. When it receives a packet
	// with the same transaction id, it knows that that is the repsonse to the original
	// message and will send it down the oneshot so the original sender can use it.
	waiters map[uint64]util.OneshotSender[MonitorResult]

	// A message is sent along this channel when an upscale is requested.
	// When the informant.NewState is called, a goroutine will be spawned that
	// just tries to receive off the channel and then request the upscale.
	// A different way to do this would be to keep a backpointer to the parent
	// `State` and then just call a method on it when an upscale is requested.
	notifier chan<- struct{}

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
func NewDispatcher(logger *zap.Logger, addr string, notifier chan<- struct{}) (disp Dispatcher, _ error) {
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
		Conn:     c,
		notifier: notifier,
		waiters:  make(map[uint64]util.OneshotSender[MonitorResult]),
		counter:  0,
		logger:   logger.Named("dispatcher"),
	}
	return disp, nil
}

// Send a packet down the connection.
func (disp *Dispatcher) send(ctx context.Context, p Packet) error {
	disp.logger.Debug("Sending packet", zap.Any("packet", p))
	return wsjson.Write(ctx, disp.Conn, p)
}

// Try to receive a packet off the connection.
func (disp *Dispatcher) recv(ctx context.Context) (*Packet, error) {
	var p Packet
	disp.logger.Debug("Reading packet off connection.")
	err := wsjson.Read(ctx, disp.Conn, &p)
	if err != nil {
		return nil, err
	}
	disp.logger.Debug("Received packet", zap.Any("packet", p))
	return &p, nil
}

// Make a request to the monitor. The dispatcher will handle returning a response
// on the provided oneshot.
//
// *Note*: sending a RequestUpscale to the monitor is incorrect. The monitor does
// not (and should) not know how to handle this and will panic. Likewise, we panic
// upon receiving a TryDownscale or NotifyUpscale request.
func (disp *Dispatcher) Call(ctx context.Context, sender util.OneshotSender[MonitorResult], req Request) {
	id := disp.counter
	disp.counter += 1
	packet := Packet{
		Stage: Stage{
			Request:  &req,
			Response: nil,
			Done:     nil,
		},
		Id: id,
	}
	err := disp.send(ctx, packet)
	if err != nil {
		disp.logger.Warn("Failed to send packet.", zap.Any("packet", packet))
	}
	disp.waiters[id] = sender
}

// Handle a request. In reality, the only request we should ever receive is
// a request upscale, in which we sent an event down the dispatcher's notifier
// channel.
func (disp *Dispatcher) handleRequest(req Request) {
	if req.RequestUpscale != nil {
		disp.logger.Info("Received request for upscale")
		// The goroutine listening on the other side will make the request
		disp.notifier <- struct{}{}
	} else if req.NotifyUpscale != nil {
		panic("informant should never receive a NotifyUpscale request from monitor")
	} else if req.TryDownscale != nil {
		panic("informant should never receive a TryDownscale request from monitor")
	} else {
		// This is a serialization error
		panic("all fields nil")
	}
}

// Handle a response. This basically means sending back the result on the channel
// that the original requester gave us.
func (disp *Dispatcher) handleResponse(res Response, id uint64) {
	if res.DownscaleResult != nil {
		// Loop up the waiter and send back the result
		sender, ok := disp.waiters[id]
		if ok {
			disp.logger.Info("Received DownscaleResult. Notifying receiver.", zap.Uint64("id", id))
			sender.Send(MonitorResult{Result: res.DownscaleResult, Confirmation: struct{}{}})
			// Don't forget to delete the waiter
			delete(disp.waiters, id)
		} else {
			panic("Received response for id without a registered sender")
		}
	} else if res.ResourceConfirmation != nil {
		// Loop up the waiter and send back the result
		sender, ok := disp.waiters[id]
		if ok {
			disp.logger.Info("Received ResourceConfirmation. Notifying receiver.", zap.Uint64("id", id))
			//
			sender.Send(MonitorResult{Result: nil, Confirmation: struct{}{}})
			// Don't forget to delete the waiter
			delete(disp.waiters, id)
		} else {
			panic("Received response for id without a registered sender")
		}
	} else if res.UpscaleResult != nil {
		panic("informant should never receive an UpscaleResult response from monitor")
	} else {
		// This is a serialization error
		panic("all fields nil")
	}
}

// Long running function that performs all orchestrates all requests/responses.
func (disp *Dispatcher) run() {
	disp.logger.Info("Starting.")
	ctx := context.Background()
	for {
		packet, err := disp.recv(ctx)
		if err != nil {
			disp.logger.Warn("Error receiving from ws connection. Continuing.", zap.Error(err))
			continue
		}
		stage := packet.Stage
		id := packet.Id
		if stage.Request != nil {
			disp.handleRequest(*stage.Request)

			// We actually send a done here because there's nothing more we can
			// do, since the request could only be a RequestUpscale. If the agent
			// decides to upscale, it will go to the monitor as a Request{NotifyUpscale}
			err := disp.send(ctx, Done(id))
			if err != nil {
				disp.logger.Warn("Failed to send Done packet.")
			}
		} else if stage.Response != nil {
			disp.handleResponse(*stage.Response, id)
			err := disp.send(ctx, Done(id))
			if err != nil {
				disp.logger.Warn("Failed to send Done packet.")
			}
		} else if stage.Done != nil {
			disp.logger.Debug("Transaction finished.", zap.Uint64("id", id))
			// yay! :)
		}
	}
}
