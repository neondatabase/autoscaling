package reqchan

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// RequestChannel is a channel that can be used to send reconcile.Requests to a controller.
type RequestChannel struct {
	ch chan event.TypedGenericEvent[reconcile.Request]
}

func (r *RequestChannel) Add(req reconcile.Request) {
	r.ch <- event.TypedGenericEvent[reconcile.Request]{Object: req}
}

func (r *RequestChannel) Close() {
	close(r.ch)
}

func (r *RequestChannel) Source() source.Source {
	idHandler := handler.TypedEnqueueRequestsFromMapFunc[reconcile.Request, reconcile.Request](
		func(_ context.Context, req reconcile.Request) []reconcile.Request {
			return []reconcile.Request{req}
		},
	)
	return source.TypedChannel[reconcile.Request](r.ch, idHandler)
}

func NewRequestChannel() *RequestChannel {
	return &RequestChannel{
		ch: make(chan event.TypedGenericEvent[reconcile.Request]),
	}
}
