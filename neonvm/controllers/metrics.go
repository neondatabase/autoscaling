package controllers

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type ReconcilerMetrics struct {
	failing                        *prometheus.GaugeVec
	vmCreationToRunnerCreationTime prometheus.Histogram
	runnerCreationToVMRunningTime  prometheus.Histogram
	vmCreationToVMRunningTime      prometheus.Histogram
	vmRestartCounts                prometheus.Counter
	reconcileDuration              prometheus.HistogramVec
}

const OutcomeLabel = "outcome"

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
			[]string{"controller", OutcomeLabel},
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
		vmRestartCounts: util.RegisterMetric(metrics.Registry, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "vm_restarts_count",
				Help: "Total number of VM restarts across the cluster captured by VirtualMachine reconciler",
			},
		)),
		reconcileDuration: *util.RegisterMetric(metrics.Registry, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "reconcile_duration_seconds",
				Help:    "Time duration of reconciles",
				Buckets: buckets,
			}, []string{OutcomeLabel},
		)),
	}
	return m
}

type ReconcileOutcome string

const (
	SuccessOutcome  ReconcileOutcome = "success"
	FailureOutcome  ReconcileOutcome = "failure"
	ConflictOutcome ReconcileOutcome = "conflict"
)

func (m ReconcilerMetrics) ObserveReconcileDuration(
	outcome ReconcileOutcome,
	duration time.Duration,
) {
	m.reconcileDuration.WithLabelValues(string(outcome)).Observe(duration.Seconds())
}

type wrappedReconciler struct {
	ControllerName string
	Reconciler     reconcile.Reconciler
	Metrics        ReconcilerMetrics

	lock        sync.Mutex
	failing     map[client.ObjectKey]struct{}
	conflicting map[client.ObjectKey]struct{}
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

	// Conflicting is the list of objects currently failing to reconcile
	// due to a conflict
	Conflicting []string `json:"conflicting"`
}

// WithMetrics wraps a given Reconciler with metrics capabilities.
//
// The returned reconciler also provides a way to get a snapshot of the state of ongoing reconciles,
// to see the data backing the metrics.
func WithMetrics(reconciler reconcile.Reconciler, rm ReconcilerMetrics, cntrlName string) ReconcilerWithMetrics {
	return &wrappedReconciler{
		Reconciler:     reconciler,
		Metrics:        rm,
		ControllerName: cntrlName,
		lock:           sync.Mutex{},
		failing:        make(map[client.ObjectKey]struct{}),
		conflicting:    make(map[client.ObjectKey]struct{}),
	}
}

func (d *wrappedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	now := time.Now()
	res, err := d.Reconciler.Reconcile(ctx, req)
	duration := time.Since(now)

	// This part is executed sequentially since we acquire a mutex lock. It
	// should be quite fast since a mutex lock/unlock + 2 memory writes takes less
	// than 100ns. I (@shayanh) preferred to go with the simplest implementation
	// as of now. For a more performant solution, if needed, we can switch to an
	// async approach.
	d.lock.Lock()
	defer d.lock.Unlock()
	outcome := SuccessOutcome
	if err != nil {
		if errors.IsConflict(err) {
			outcome = ConflictOutcome
			d.conflicting[req.NamespacedName] = struct{}{}
		} else {
			outcome = FailureOutcome
			d.failing[req.NamespacedName] = struct{}{}

			// If the VM is now getting non-conflict errors, it probably
			// means transient conflicts has been resolved.
			//
			// Notably, the other way around is not true:
			// if a VM is getting conflict errors, it doesn't mean
			// non-conflict errors are resolved, as they are more
			// likely to be persistent.
			delete(d.conflicting, req.NamespacedName)
		}

		log.Error(err, "Failed to reconcile VirtualMachine",
			"duration", duration.String(), "outcome", outcome)
	} else {
		delete(d.failing, req.NamespacedName)
		delete(d.conflicting, req.NamespacedName)
		log.Info("Successful reconciliation", "duration", duration.String())
	}
	d.Metrics.ObserveReconcileDuration(outcome, duration)
	d.Metrics.failing.WithLabelValues(d.ControllerName,
		string(FailureOutcome)).Set(float64(len(d.failing)))
	d.Metrics.failing.WithLabelValues(d.ControllerName,
		string(ConflictOutcome)).Set(float64(len(d.conflicting)))

	return res, err
}

func keysToSlice(m map[client.ObjectKey]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k.String())
	}
	return keys
}

func (r *wrappedReconciler) Snapshot() ReconcileSnapshot {
	r.lock.Lock()
	defer r.lock.Unlock()

	failing := keysToSlice(r.failing)
	conflicting := keysToSlice(r.conflicting)

	return ReconcileSnapshot{
		ControllerName: r.ControllerName,
		Failing:        failing,
		Conflicting:    conflicting,
	}
}
