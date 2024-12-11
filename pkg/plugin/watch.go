package plugin

// Helper functions to set up the persistent listening for watch events.

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coreclient "k8s.io/client-go/kubernetes"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	"github.com/neondatabase/autoscaling/pkg/plugin/initevents"
	"github.com/neondatabase/autoscaling/pkg/plugin/reconcile"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

// watchHandlers builds the default handler functions to bridge between the k8s watch events and the
// plugin's internal reconcile queue.
func watchHandlers[P reconcile.Object](
	queue *reconcile.Queue,
	inits *initevents.InitEventsMiddleware,
) watch.HandlerFuncs[P] {
	return watch.HandlerFuncs[P]{
		AddFunc: func(obj P, preexisting bool) {
			if preexisting && inits != nil {
				inits.AddRequired(obj)
			}
			queue.Enqueue(reconcile.EventKindAdded, obj)
		},
		UpdateFunc: func(oldObj, newObj P) {
			queue.Enqueue(reconcile.EventKindModified, newObj)
		},
		DeleteFunc: func(obj P, mayBeStale bool) {
			queue.Enqueue(reconcile.EventKindDeleted, obj)
		},
	}
}

// helper function
func onlyErr[T any](_ T, err error) error {
	return err
}

func watchConfig[T any](metrics watch.Metrics) watch.Config {
	sampleObj := any(new(T)).(runtime.Object)
	gvk, err := util.LookupGVKForType(sampleObj)
	if err != nil {
		panic(err)
	}
	kind := gvk.Kind

	return watch.Config{
		ObjectNameLogField: kind,
		Metrics: watch.MetricsConfig{
			Metrics:  metrics,
			Instance: fmt.Sprint(kind, "s"),
		},
		// FIXME: make these configurable.
		RetryRelistAfter: util.NewTimeRange(time.Second, 3, 5),
		RetryWatchAfter:  util.NewTimeRange(time.Second, 3, 5),
	}
}

func watchNodeEvents(
	ctx context.Context,
	parentLogger *zap.Logger,
	client coreclient.Interface,
	metrics watch.Metrics,
	callbacks watch.HandlerFuncs[*corev1.Node],
) (*watch.Store[corev1.Node], error) {
	return watch.Watch(
		ctx,
		parentLogger.Named("watch-nodes"),
		client.CoreV1().Nodes(),
		watchConfig[corev1.Node](metrics),
		watch.Accessors[*corev1.NodeList, corev1.Node]{
			Items: func(list *corev1.NodeList) []corev1.Node { return list.Items },
		},
		watch.InitModeSync,
		metav1.ListOptions{},
		callbacks,
	)
}

func watchPodEvents(
	ctx context.Context,
	parentLogger *zap.Logger,
	client coreclient.Interface,
	metrics watch.Metrics,
	callbacks watch.HandlerFuncs[*corev1.Pod],
) (*watch.Store[corev1.Pod], error) {
	return watch.Watch(
		ctx,
		parentLogger.Named("watch-pods"),
		client.CoreV1().Pods(corev1.NamespaceAll),
		watchConfig[corev1.Pod](metrics),
		watch.Accessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		watch.InitModeSync,
		metav1.ListOptions{},
		callbacks,
	)
}

func watchMigrationEvents(
	ctx context.Context,
	parentLogger *zap.Logger,
	client vmclient.Interface,
	metrics watch.Metrics,
	callbacks watch.HandlerFuncs[*vmv1.VirtualMachineMigration],
) error {
	return onlyErr(watch.Watch(
		ctx,
		parentLogger.Named("watch-migrations"),
		client.NeonvmV1().VirtualMachineMigrations(corev1.NamespaceAll),
		watchConfig[vmv1.VirtualMachineMigration](metrics),
		watch.Accessors[*vmv1.VirtualMachineMigrationList, vmv1.VirtualMachineMigration]{
			Items: func(list *vmv1.VirtualMachineMigrationList) []vmv1.VirtualMachineMigration { return list.Items },
		},
		watch.InitModeSync,
		metav1.ListOptions{
			// NB: Including just the label itself means that we select for objects that *have* the
			// label, without caring about the actual value.
			//
			// See also:
			// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#set-based-requirement
			LabelSelector: LabelPluginCreatedMigration,
		},
		callbacks,
	))
}
