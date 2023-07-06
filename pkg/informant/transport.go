package informant

// Defines types that are used to communicate with the monitor over websocket
// connection. Carefully ensure that serialization is compatible with transport
// types defined in the monitor.

import (
	"github.com/neondatabase/autoscaling/pkg/api"
)

// See monitor docs for a more thorough explanation of the monitor protocol. In
// short, one party sends a `Request`, they are responded to with a `Response`,
// and then they send a `Done` to finish off the transaction. The only thing
// sent over the network is the `Packet` struct.
//
// These types might look a little funky . . . that's because go doesn't have
// enums. On the rust side, `Stage`, `Request` and `Response` are all enums.
// The way deserialization works is that the only the specific struct field
// that matches the enum variant is serialized out. The other pointers are set
// to nil. Thus, a Request(RequestUpscale) would be deserialized as
//
// Request{
//     RequestUpscale 0xdeadbeef
//     NotifyUpscale  0x0
//     TryDownscale   0x0
// }
//
// This is achieved using the `omitempty` struct tag.

type Packet struct {
	Stage Stage  `json:"stage"`
	Id    uint64 `json:"id"`
}

type Stage struct {
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
	Done     *struct{} `json:"done,omitempty"`
}

type Request struct {
	RequestUpscale *struct{}  `json:"requestUpscale,omitempty"`
	NotifyUpscale  *Resources `json:"notifyUpscale,omitempty"`
	TryDownscale   *Resources `json:"tryDownscale,omitempty"`
}

type Response struct {
	UpscaleResult        *Resources       `json:"upscaleResult,omitempty"`
	ResourceConfirmation *struct{}        `json:"resourceConfirmation,omitempty"`
	DownscaleResult      *DownscaleResult `json:"downscaleResult,omitempty"`
}

type Resources struct {
	Cpu uint64 `json:"cpu"`
	Mem uint64 `json:"mem"`
}

type DownscaleResult struct {
	Ok     bool   `json:"ok"`
	Status string `json:"status"`
}

// Convert into api.DownscaleResult.
//
// The reason for having two types is to prevent us from having to keep track/control
// how api.DownscaleResult is serialized
func (res *DownscaleResult) Into() *api.DownscaleResult {
	return &api.DownscaleResult{
		Ok:     res.Ok,
		Status: res.Status,
	}
}

func Done(id uint64) Packet {
	return Packet{
		Stage: Stage{
			Request:  nil,
			Response: nil,
			Done:     &struct{}{},
		},
		Id: id,
	}
}
