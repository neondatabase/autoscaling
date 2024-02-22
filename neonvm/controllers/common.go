package controllers

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

func SetupReconciler(
	mgr manager.Manager,
	name string,
	config *ReconcilerConfig,
	metrics ReconcilerMetrics,
) (ReconcilerWithMetrics, error) {
	client := mgr.GetClient()
	scheme := mgr.GetScheme()
	recorder := mgr.GetEventRecorderFor(fmt.Sprintf("%s-controller", name))

	var reconciler reconcile.Reconciler
	var typ ctrlclient.Object

	switch name {
	case "virtualmachine":
		typ = &vmv1.VirtualMachine{}
		reconciler = &VirtualMachineReconciler{
			Client:   client,
			Scheme:   scheme,
			Recorder: recorder,
			Config:   config,
			Metrics:  metrics,
		}
	case "virtualmachinemigration":
		typ = &vmv1.VirtualMachineMigration{}
		reconciler = &VirtualMachineMigrationReconciler{
			Client:   client,
			Scheme:   scheme,
			Recorder: recorder,
			Config:   config,
			Metrics:  metrics,
		}
	default:
		panic(fmt.Errorf("unknown reconciler name %q", name))
	}

	reconcilerWithMetrics := WithMetrics(reconciler, metrics, name)
	err := ctrl.NewControllerManagedBy(mgr).
		For(typ).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: config.MaxConcurrentReconciles}).
		Named(name).
		Complete(reconcilerWithMetrics)

	return reconcilerWithMetrics, err
}
