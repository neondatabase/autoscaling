package controllers

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type ReconcilerMetrics struct {
	failing                        *prometheus.GaugeVec
	vmCreationToRunnerCreationTime prometheus.Histogram
	runnerCreationToVMRunningTime  prometheus.Histogram
	vmCreationToVMRunningTime      prometheus.Histogram
}

func MakeReconcilerMetrics() ReconcilerMetrics {
	// Copied bucket values from controller runtime latency metric. We can
	// adjust them in the future if needed.
	buckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
		1.25, 1.5, 1.75, 2.0, 2.5, 3.0, 3.5, 4.0, 4.5, 5, 6, 7, 8, 9, 10, 15, 20, 25, 30, 40, 50, 60}

	m := ReconcilerMetrics{
		failing: util.RegisterMetric(metrics.Registry, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "reconcile_failing_objects",
				Help: "Number of objects that are failing to reconcile for each specific controller",
			},
			[]string{"controller"},
		)),
		vmCreationToRunnerCreationTime: util.RegisterMetric(metrics.Registry, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "vm_creation_to_runner_creation_duration_seconds",
				Help:    "Time duration from VirtualMachine.CreationTimestamp to runner Pod.CreationTimestamp",
				Buckets: buckets,
			},
		)),
		runnerCreationToVMRunningTime: util.RegisterMetric(metrics.Registry, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "vm_runner_creation_to_vm_running_duration_seconds",
				Help:    "Time duration from runner Pod.CreationTimestamp to the moment when VirtualMachine.Status.Phase becomes Running",
				Buckets: buckets,
			},
		)),
		vmCreationToVMRunningTime: util.RegisterMetric(metrics.Registry, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "vm_creation_to_vm_running_duration_seconds",
				Help:    "Time duration from VirtualMachine.CreationTimeStamp to the moment when VirtualMachine.Status.Phase becomes Running",
				Buckets: buckets,
			},
		)),
	}
	return m
}

type wrappedReconciler struct {
	ControllerName string
	Reconciler     reconcile.Reconciler
	Metrics        ReconcilerMetrics

	lock    sync.Mutex
	failing map[client.ObjectKey]struct{}
}

// ReconcilerWithMetrics is a Reconciler produced by WithMetrics that can return a snapshot of the
// state backing the metrics.
type ReconcilerWithMetrics interface {
	reconcile.Reconciler

	Snapshot() ReconcileSnapshot
}

// ReconcileSnapshot provides a glimpse into the current state of ongoing reconciles
//
// This type is (transitively) returned by the controller's "dump state" HTTP endpoint, and exists
// to allow us to get deeper information on the metrics - we can't expose information for every
// VirtualMachine into the metrics (it'd be too high cardinality), but we *can* make it available
// when requested.
type ReconcileSnapshot struct {
	// ControllerName is the name of the controller: virtualmachine or virtualmachinemigration.
	ControllerName string `json:"controllerName"`

	// Failing is the list of objects currently failing to reconcile
	Failing []string `json:"failing"`
}

// WithMetrics wraps a given Reconciler with metrics capabilities, also returning a function that
// produces a snapshot of the metrics, for easier inspection.
func WithMetrics(
	reconciler reconcile.Reconciler,
	rm ReconcilerMetrics,
	cntrlName string,
) ReconcilerWithMetrics {
	r := &wrappedReconciler{
		Reconciler:     reconciler,
		Metrics:        rm,
		ControllerName: cntrlName,
		lock:           sync.Mutex{},
		failing:        make(map[client.ObjectKey]struct{}),
	}

	return r
}

func (d *wrappedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := d.Reconciler.Reconcile(ctx, req)

	// This part is executed sequentially since we acquire a mutex lock. It
	// should be quite fast since a mutex lock/unlock + 2 memory writes takes less
	// than 100ns. I (@shayanh) preferred to go with the simplest implementation
	// as of now. For a more performant solution, if needed, we can switch to an
	// async approach.
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

func (r *wrappedReconciler) Snapshot() ReconcileSnapshot {
	r.lock.Lock()
	defer r.lock.Unlock()

	failing := make([]string, 0, len(r.failing))
	for namespacedName := range r.failing {
		failing = append(failing, namespacedName.String())
	}

	return ReconcileSnapshot{
		ControllerName: r.ControllerName,
		Failing:        failing,
	}
}
