package controllers

import (
	"context"
	"fmt"
	"runtime/debug"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type catchPanicReconciler struct {
	inner reconcile.Reconciler
}

func withCatchPanic(r reconcile.Reconciler) reconcile.Reconciler {
	return &catchPanicReconciler{inner: r}
}

func (r *catchPanicReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log := log.FromContext(ctx)

	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panicked with: %v", v)
			log.Error(err, "Reconcile panicked", "stack", string(debug.Stack()))
		}
	}()

	result, err = r.inner.Reconcile(ctx, req)
	return
}
