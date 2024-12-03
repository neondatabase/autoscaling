package plugin

// Root state for the plugin.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	vmclient "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/plugin/metrics"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
	"github.com/neondatabase/autoscaling/pkg/util/patch"
	"github.com/neondatabase/autoscaling/pkg/util/watch"
)

// PluginState stores the state of the scheduler plugin in its entirety
type PluginState struct {
	mu sync.Mutex

	config Config

	nodes map[string]*nodeState

	// tentativelyScheduled stores the UIDs of pods that have been approved for final scheduling
	// with the Reserve plugin method, but haven't yet been processed internally as finally being
	// assigned to those nodes.
	//
	// the string associated with each pod is the name of the node.
	tentativelyScheduled map[types.UID]string

	startupDone         bool
	requeueAfterStartup map[types.UID]struct{}

	// maxNodeCPU is the maximum amount of CPU we've seen available for a node.
	// We use this when scoring pod placements.
	maxNodeCPU vmv1.MilliCPU
	// maxNodeMem is the maximum amount of memory we've seen available for a node.
	// We use this when scoring pod placements.
	maxNodeMem api.Bytes

	metrics metrics.Plugin

	requeuePod      func(uid types.UID) error
	requeueNode     func(nodeName string) error
	createMigration func(*zap.Logger, *vmv1.VirtualMachineMigration) error
	deleteMigration func(*zap.Logger, *vmv1.VirtualMachineMigration) error
	patchVM         func(util.NamespacedName, []patch.Operation) error
}

type nodeState struct {
	node *state.Node

	// requestedMigrations stores the set of pods that we've decided we should migrate.
	//
	// When they are reconciled, we will (a) double-check that we should still migrate them, and (b)
	// if so, create a VirtualMachineMigration object to handle it.
	requestedMigrations map[types.UID]struct{}

	// podsVMPatchedAt stores the last time that the VirtualMachine object for a Pod was patched, so
	// that we can avoid spamming patch requests if the Pod is just slightly out of date.
	//
	// The map is keyed by the *Pod* UID, even though it stores when we patched the *VM*.
	podsVMPatchedAt map[types.UID]time.Time
}

func NewPluginState(
	config Config,
	vmClient vmclient.Interface,
	reg prometheus.Registerer,
	podWatchStore *watch.Store[corev1.Pod],
	nodeWatchStore *watch.Store[corev1.Node],
) *PluginState {
	crudTimeout := time.Second * time.Duration(config.K8sCRUDTimeoutSeconds)

	indexedNodeStore := watch.NewIndexedStore(nodeWatchStore, watch.NewFlatNameIndex[corev1.Node]())

	metrics := metrics.BuildPluginMetrics(config.NodeMetricLabels, reg)

	return &PluginState{
		mu: sync.Mutex{},

		config: config,

		nodes:                make(map[string]*nodeState),
		tentativelyScheduled: make(map[types.UID]string),

		startupDone:         false,
		requeueAfterStartup: make(map[types.UID]struct{}),

		// these values will be set as we handle node events:
		maxNodeCPU: 0,
		maxNodeMem: 0,

		metrics: metrics,
		requeuePod: func(uid types.UID) error {
			ok := podWatchStore.NopUpdate(uid)
			if !ok {
				return errors.New("pod not found in watch store")
			}
			return nil
		},
		requeueNode: func(nodeName string) error {
			node, ok := indexedNodeStore.GetIndexed(
				func(i *watch.FlatNameIndex[corev1.Node]) (*corev1.Node, bool) {
					return i.Get(nodeName)
				},
			)
			if !ok {
				return errors.New("node not found in watch store")
			}

			_ = nodeWatchStore.NopUpdate(node.UID)
			return nil
		},
		createMigration: func(logger *zap.Logger, vmm *vmv1.VirtualMachineMigration) error {
			ctx, cancel := context.WithTimeout(context.TODO(), crudTimeout)
			defer cancel()

			_, err := vmClient.NeonvmV1().VirtualMachineMigrations(vmm.Namespace).
				Create(ctx, vmm, metav1.CreateOptions{})
			metrics.RecordK8sOp("Create", "VirtualMachineMigration", vmm.Name, err)
			if err != nil && apierrors.IsAlreadyExists(err) {
				logger.Warn("Migration already exists for this pod")
				return nil
			}
			return err
		},
		deleteMigration: func(logger *zap.Logger, vmm *vmv1.VirtualMachineMigration) error {
			ctx, cancel := context.WithTimeout(context.TODO(), crudTimeout)
			defer cancel()

			opts := metav1.DeleteOptions{
				// Include the extra pre-condition that we're deleting exactly the migration object
				// that was specified.
				Preconditions: &metav1.Preconditions{
					UID:             &vmm.UID,
					ResourceVersion: nil,
				},
			}

			err := vmClient.NeonvmV1().VirtualMachineMigrations(vmm.Namespace).
				Delete(ctx, vmm.Name, opts)
			metrics.RecordK8sOp("Delete", "VirtualMachineMigration", vmm.Name, err)
			return err
		},
		patchVM: func(vm util.NamespacedName, patches []patch.Operation) error {
			patchPayload, err := json.Marshal(patches)
			if err != nil {
				panic(fmt.Errorf("could not marshal JSON patch: %w", err))
			}

			ctx, cancel := context.WithTimeout(context.TODO(), crudTimeout)
			defer cancel()

			_, err = vmClient.NeonvmV1().VirtualMachines(vm.Namespace).
				Patch(ctx, vm.Name, types.JSONPatchType, patchPayload, metav1.PatchOptions{})
			metrics.RecordK8sOp("Patch", "VirtualMachine", vm.Name, err)
			return err
		},
	}
}
