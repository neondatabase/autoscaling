package main

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"

	"github.com/neondatabase/autoscaling/pkg/reporting"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

func startK8sWatches(
	ctx context.Context,
	logger *zap.Logger,
	client clientset.Interface,
	reg prometheus.Registerer,
	sink *reporting.EventSink[TraceEvent],
	keepPodLabels []string,
	keepNodeLabels []string,
) error {
	metrics := watch.NewMetrics("autoscaling_trace_collector_watch", reg)

	podsLogger := logger.Named("watch-pods")
	_, err := watch.Watch(
		ctx,
		podsLogger.Named("watch"),
		client.CoreV1().Pods(corev1.NamespaceAll),
		watch.Config{
			ObjectNameLogField: "Pod",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "Pods",
			},
			RetryRelistAfter: util.NewTimeRange(time.Millisecond, 500, 1000),
			RetryWatchAfter:  util.NewTimeRange(time.Millisecond, 500, 1000),
		},
		watch.Accessors[*corev1.PodList, corev1.Pod]{
			Items: func(list *corev1.PodList) []corev1.Pod { return list.Items },
		},
		watch.InitModeDefer,
		metav1.ListOptions{},
		watchHandlers(podsLogger, true, keepPodLabels, sink, func(event *TraceEvent, pod *corev1.Pod) error {
			podData, err := ExtractPodData(pod)
			if err != nil {
				return err
			}
			event.PodData = podData
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to start pod watcher: %w", err)
	}

	nodesLogger := logger.Named("watch-nodes")
	_, err = watch.Watch(
		ctx,
		nodesLogger.Named("watch"),
		client.CoreV1().Nodes(),
		watch.Config{
			ObjectNameLogField: "Node",
			Metrics: watch.MetricsConfig{
				Metrics:  metrics,
				Instance: "Nodes",
			},
			RetryRelistAfter: util.NewTimeRange(time.Millisecond, 500, 1000),
			RetryWatchAfter:  util.NewTimeRange(time.Millisecond, 500, 1000),
		},
		watch.Accessors[*corev1.NodeList, corev1.Node]{
			Items: func(list *corev1.NodeList) []corev1.Node { return list.Items },
		},
		watch.InitModeDefer,
		metav1.ListOptions{},
		watchHandlers(nodesLogger, false, keepNodeLabels, sink, func(event *TraceEvent, node *corev1.Node) error {
			event.NodeData = ExtractNodeData(node)
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to start node watcher: %w", err)
	}

	return nil
}

func watchHandlers[T interface {
	metav1.ObjectMetaAccessor
	runtime.Object
}](
	logger *zap.Logger,
	namespaced bool,
	keepLabels []string,
	sink *reporting.EventSink[TraceEvent],
	setObjData func(*TraceEvent, T) error,
) watch.HandlerFuncs[T] {
	baseEvent := func(now time.Time, obj T, kind EventKind) TraceEvent {
		ts := metav1.NewTime(now)

		meta := obj.GetObjectMeta()
		name := meta.GetName()
		if namespaced {
			ns := meta.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			name = fmt.Sprint(ns, "/", name)
		}
		uid := meta.GetUID()

		eventLabels := make(map[string]string)
		objLabels := meta.GetLabels()
		for _, label := range keepLabels {
			if value, ok := objLabels[label]; ok {
				eventLabels[label] = value
			}
		}

		deletionRequested := meta.GetDeletionTimestamp() != nil

		objKind := obj.GetObjectKind().GroupVersionKind().Kind

		return TraceEvent{
			Timestamp:         ts,
			EventKind:         kind,
			ObjectKind:        objKind,
			Name:              name,
			UID:               string(uid),
			Labels:            eventLabels,
			Preexisting:       nil, // Will be set by caller, only on Add
			DeletionRequested: deletionRequested,
			PodData:           nil, // Set by caller
			NodeData:          nil, // Set by caller
			ErrorData:         nil, // Set by caller
		}
	}

	submitError := func(obj T, err error, preexisting *bool) {
		event := baseEvent(time.Now(), obj, EventError)
		event.Preexisting = preexisting
		event.ErrorData = &ErrorData{
			Error: err.Error(),
		}

		logger.Error(
			fmt.Sprintf("Failed to extract object data from %s", event.ObjectKind),
			zap.String("name", event.Name),
			zap.Error(err),
		)
		sink.Enqueue(event)
	}

	return watch.HandlerFuncs[T]{
		AddFunc: func(obj T, preexisting bool) {
			event := baseEvent(time.Now(), obj, EventCreated)
			event.Preexisting = &preexisting

			err := setObjData(&event, obj)
			if err != nil {
				submitError(obj, err, &preexisting)
				return
			}

			sink.Enqueue(event)
		},
		UpdateFunc: func(oldObj T, newObj T) {
			now := time.Now()
			oldEvent := baseEvent(now, oldObj, EventModified)
			newEvent := baseEvent(now, newObj, EventModified)

			oldErr := setObjData(&oldEvent, oldObj)
			newErr := setObjData(&newEvent, newObj)

			// emit an error if EITHER it's newly erroring, or it's already erroring but the error
			// message changed.
			emitError := (oldErr == nil && newErr != nil) ||
				(oldErr != nil && newErr != nil && oldErr.Error() != newErr.Error())

			if newErr != nil {
				if emitError {
					submitError(newObj, newErr, nil)
				}
				return
			}

			// Emit this event only if newEvent != oldEvent
			if !reflect.DeepEqual(oldEvent, newEvent) {
				sink.Enqueue(newEvent)
			}
		},
		DeleteFunc: func(obj T, mayBeStale bool) {
			event := baseEvent(time.Now(), obj, EventCreated)

			err := setObjData(&event, obj)
			if err != nil {
				submitError(obj, err, nil)
				return
			}

			sink.Enqueue(event)
		},
	}
}
