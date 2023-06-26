package informant

import (
	"context"
	"fmt"
	"time"

	"github.com/neondatabase/autoscaling/pkg/util"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type MonitorResult struct {
	Result       DownscaleResult
	Confirmation struct{}
}

// TODO: do we need synchronization?
type Dispatcher struct {
	Conn    *websocket.Conn
	ctx     context.Context

    // When someone sends a message, the dispatcher will attach a transaction id
    // to it so that it knows when a response is back. When it receives the response
    // it will send it down the channel so the original sender can use it.
	waiters map[uint64]util.OneshotSender[MonitorResult]

    // A message is sent along this channel when an upscale is requested
    notifier chan<- struct{}

	// Only we care about this number. The other side will just send back packets
	// with the id of the request, but they never do anything with the number.
	counter uint64
	logger  *zap.Logger
}

func NewDispatcher(addr string, logger *zap.Logger, notifier chan<- struct{}) (disp Dispatcher, _ error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	c, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		return disp, fmt.Errorf("Error creating dispatcher: %v", err)
	}
	disp = Dispatcher{
		Conn:    c,
		ctx:     ctx,
        notifier: notifier,
		waiters: make(map[uint64]util.OneshotSender[MonitorResult]),
		counter: 0,
		logger:  logger.Named("dispatcher"),
	}
	return disp, nil
}

func (disp *Dispatcher) send(p Packet) error {
	return wsjson.Write(disp.ctx, disp.Conn, p)
}

func (disp *Dispatcher) recv() (*Packet, error) {
	var p Packet
	err := wsjson.Read(disp.ctx, disp.Conn, &p)
	if err != nil {
		return &p, nil
	}
	return &p, nil
}

func (disp *Dispatcher) Call(stage Stage, sender util.OneshotSender[MonitorResult]) {
	id := disp.counter
	disp.counter += 1
    disp.send(Packet{Stage: stage, SeqNum: id})
	disp.waiters[id] = sender
}

func (disp *Dispatcher) run() {
	for {
		packet, err := disp.recv()
		if err != nil {
			disp.logger.Warn("Error receiving from ws connection. Continuing.", zap.Error(err))
			continue
		}
		stage := packet.Stage
		id := packet.SeqNum
		switch {
		case stage.Request != nil:
			{
				req := stage.Request
				switch {
				case req.RequestUpscale != nil:
					{
                        disp.notifier <- struct{}{}
					}
				case req.NotifyUpscale != nil:
					{
						panic("informant should never receive a NotifyUpscale request from monitor")
					}
				case req.TryDownscale != nil:
					{
						panic("informant should never receive a TryDownscale request from monitor")
					}
				default:
					{
						panic("all fields nil")
					}
				}
			}
		case stage.Response != nil:
			{
				res := stage.Response
				switch {
				case res.DownscaleResult != nil:
					{
						sender, ok := disp.waiters[id]
						if ok {
                            sender.Send(MonitorResult{Result: *res.DownscaleResult})
							delete(disp.waiters, id)
							disp.send(Done())
						} else {
                            panic("Received response for id without a registered sender")
						}
					}
				case res.ResourceConfirmation != nil:
					{
						sender, ok := disp.waiters[id]
						if ok {
                            sender.Send(MonitorResult{Result: *res.DownscaleResult})
							delete(disp.waiters, id)
							disp.send(Done())
						} else {
                            panic("Received response for id without a registered sender")
						}
						disp.send(Done())
					}
				case res.UpscaleResult != nil:
					{
						panic("informant should never receive an UpscaleResult response from monitor")
					}
				default:
					{
						panic("all fields nil")
					}
				}
			}
		case stage.Done != nil:
			{
				// yay! :)
			}
		}
	}
}
