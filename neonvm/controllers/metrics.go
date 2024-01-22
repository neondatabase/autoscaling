package controllers

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type ReconcilerMetrics struct {
	failing *prometheus.GaugeVec
}

func MakeReconcilerMetrics() ReconcilerMetrics {
	m := ReconcilerMetrics{
		failing: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "failing",
				Help: "number of failing objects",
			},
			[]string{"controller"},
		),
	}
	metrics.Registry.MustRegister(m.failing)
	return m
}

type wrappedReconciler struct {
	ControllerName string
	Reconciler     reconcile.Reconciler
	Metrics        ReconcilerMetrics

	lock    sync.Mutex
	failing map[client.ObjectKey]struct{}
}

func WithMetrics(reconciler reconcile.Reconciler, rm ReconcilerMetrics, cntrlName string) reconcile.Reconciler {
	return &wrappedReconciler{
		ControllerName: cntrlName,
		Reconciler:     reconciler,
		Metrics:        rm,
		lock:           sync.Mutex{},
		failing:        make(map[client.ObjectKey]struct{}),
	}
}

func (d *wrappedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := d.Reconciler.Reconcile(ctx, req)

	d.lock.Lock()
	defer d.lock.Unlock()
	if err != nil {
		d.failing[req.NamespacedName] = struct{}{}
	} else {
		delete(d.failing, req.NamespacedName)
	}
	d.Metrics.failing.WithLabelValues(d.ControllerName).Set(float64(len(d.failing)))

	return res, err
}
