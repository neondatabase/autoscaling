package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fields "k8s.io/apimachinery/pkg/fields"
	klog "k8s.io/klog/v2"

	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

//////////////////
// CONFIG TYPES //
//////////////////

type config struct {
	// NodeDefaults is the default node configuration used when not overridden
	NodeDefaults nodeConfig `json:"nodeDefaults"`
	// NodeOverrides is a list of node configurations that override the default for a small set of
	// nodes.
	//
	// Duplicate names are allowed, and earlier overrides take precedence over later ones.
	NodeOverrides []overrideSet `json:"nodeOverrides"`
	// MemSlotSize is the smallest unit of memory that the scheduler plugin will reserve for a VM,
	// and is defined globally.
	//
	// Any VM with a MemorySlotSize that does not match this value will be rejected.
	//
	// This value cannot be increased at runtime, and configurations that attempt to do so will be
	// rejected.
	MemSlotSize resource.Quantity `json:"memBlockSize"`

	// SchedulerName informs the scheduler of its name, so that it can identify pods that a previous
	// version handled.
	SchedulerName string `json:"schedulerName"`

	// DoMigration, if provided, allows VM migration to be disabled
	//
	// This flag is intended to be temporary, just until NeonVM supports mgirations and we can
	// re-enable it.
	DoMigration *bool `json:"doMigration"`

	// JSONString is the JSON string that was used to generate this config struct
	JSONString string `json:"-"`
}

type overrideSet struct {
	Nodes  []string   `json:"nodes"` // TODO: these should be changed to globs in the future.
	Config nodeConfig `json:"config"`
}

type nodeConfig struct {
	Cpu         resourceConfig `json:"cpu"`
	Memory      resourceConfig `json:"memory"`
	ComputeUnit api.Resources  `json:"computeUnit"`
}

// resourceConfig configures the amount of a particular resource we're willing to allocate to VMs,
// both the soft limit (Watermark) and the hard limit (via System)
type resourceConfig struct {
	// Watermark is the fraction of non-system resource allocation above which we should be
	// migrating VMs away to reduce usage
	//
	// If empty, the watermark is set as equal to the "hard" limit from system resources.
	//
	// The word "watermark" was originally used by @zoete as a temporary stand-in term during a
	// meeting, and so it has intentionally been made permanent to spite the concept of "temporary" 😛
	Watermark float32 `json:"watermark,omitempty"`
	// System is the absolute amount of the resource allocated to non-user node functions, like
	// Kubernetes daemons
	System resource.Quantity `json:"system,omitempty"`
}

func (c *config) migrationEnabled() bool {
	return c.DoMigration == nil || *c.DoMigration
}

///////////////////////
// CONFIG VALIDATION //
///////////////////////

// if the returned error is not nil, the string is a JSON path to the invalid value
func (c *config) validate() (string, error) {
	if path, err := c.NodeDefaults.validate(); err != nil {
		return fmt.Sprintf("nodeDefaults.%s", path), err
	}

	for i, override := range c.NodeOverrides {
		if path, err := override.validate(); err != nil {
			return fmt.Sprintf("nodeOverrides[%d].%s", i, path), err
		}
	}

	if c.MemSlotSize.Value() <= 0 {
		return "memBlockSize", errors.New("value must be > 0")
	} else if c.MemSlotSize.Value() <= math.MaxInt64/1000 && c.MemSlotSize.MilliValue()%1000 != 0 {
		return "memBlockSize", errors.New("value cannot have milli-precision")
	}

	if c.SchedulerName == "" {
		return "schedulerName", errors.New("string cannot be empty")
	}

	return "", nil
}

func (s *overrideSet) validate() (string, error) {
	if len(s.Nodes) == 0 {
		return "nodes", errors.New("array must be non-empty")
	}

	if path, err := s.Config.validate(); err != nil {
		return fmt.Sprintf("config.%s", path), err
	}

	return "", nil
}

func (c *nodeConfig) validate() (string, error) {
	if path, err := c.Cpu.validate(false); err != nil {
		return fmt.Sprintf("cpu.%s", path), err
	}
	if path, err := c.Memory.validate(true); err != nil {
		return fmt.Sprintf("memory.%s", path), err
	}
	if err := c.ComputeUnit.ValidateNonZero(); err != nil {
		return "computeUnit", err
	}

	return "", nil
}

func (c *resourceConfig) validate(isMemory bool) (string, error) {
	if c.Watermark <= 0.0 {
		return "watermark", errors.New("value must be > 0")
	} else if c.Watermark > 1.0 {
		return "watermark", errors.New("value must be <= 1")
	}

	if c.System.Value() <= 0 {
		return "system", errors.New("value must be > 0")
	} else if isMemory && c.System.Value() < math.MaxInt64 && c.System.MilliValue()%1000 != 0 {
		return "system", errors.New("value cannot have milli-precision")
	}

	return "", nil
}

func (oldConf *config) validateChangeTo(newConf *config) (string, error) {
	if newConf.MemSlotSize != oldConf.MemSlotSize {
		return "memSlotSize", errors.New("value cannot be changed at runtime")
	}
	if newConf.SchedulerName != oldConf.SchedulerName {
		return "schedulername", errors.New("value cannot be changed at runtime")
	}

	return "", nil
}

/////////////////////////////////////////
// CONFIG UPDATE TRACKING AND HANDLING //
/////////////////////////////////////////

// setConfigAndStartWatcher basically does what it says. It (indirectly) spawns goroutines that will
// update the plugin's config (calling e.handleNewConfigMap).
func (e *AutoscaleEnforcer) setConfigAndStartWatcher(ctx context.Context) error {
	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	addEvents := make(chan *corev1.ConfigMap)
	updateEvents := make(chan *corev1.ConfigMap)

	watchStore, err := util.Watch(
		ctx,
		e.handle.ClientSet().CoreV1().ConfigMaps(ConfigMapNamespace),
		util.WatchConfig{
			LogName: fmt.Sprintf("ConfigMap %s:%s", ConfigMapNamespace, ConfigMapName),
			// We don't need to be super responsive to config updates, so we can wait 5-10 seconds.
			//
			// FIXME: make these configurable.
			RetryRelistAfter: util.NewTimeRange(time.Second, 5, 10),
			RetryWatchAfter:  util.NewTimeRange(time.Second, 5, 10),
		},
		util.WatchAccessors[*corev1.ConfigMapList, corev1.ConfigMap]{
			Items: func(list *corev1.ConfigMapList) []corev1.ConfigMap { return list.Items },
		},
		util.InitWatchModeSync,
		metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector(metav1.ObjectNameField, ConfigMapName).String(),
		},
		util.WatchHandlerFuncs[*corev1.ConfigMap]{
			// Hooking into AddFunc as well as UpdateFunc allows for us to handle both "patch" and
			// "delete + replace" workflows for the ConfigMap.
			AddFunc: func(newConf *corev1.ConfigMap, preexisting bool) {
				// preexisting ConfigMaps are handled by the call to watchStore.Items() below. If we
				// tried to send them here, we'd deadlock.
				if !preexisting {
					addEvents <- newConf
				}
			},
			UpdateFunc: func(oldConf, newConf *corev1.ConfigMap) { updateEvents <- newConf },
		},
	)
	if err != nil {
		return fmt.Errorf("Failed to watch ConfigMaps: %w", err)
	}

	initialConfigs := watchStore.Items()
	if len(initialConfigs) == 0 {
		watchStore.Stop()
		return fmt.Errorf(
			"No initial ConfigMap found (expected name = %s, namespace = %s)",
			ConfigMapName, ConfigMapNamespace,
		)
	}

	if len(initialConfigs) > 1 {
		panic(fmt.Sprintf("more than one config found for name %s:%s", ConfigMapNamespace, ConfigMapName))
	}

	if err := e.handleNewConfigMap(initialConfigs[0]); err != nil {
		watchStore.Stop()
		return fmt.Errorf("Bad initial ConfigMap: %w", err)
	}

	// Start listening on the channels that we provided to the callbacks, now that we've gotten our
	// initial ConfigMap
	listenWithName := func(ch <-chan *corev1.ConfigMap, desc string) {
		for newConf := range ch {
			// Wrap this in a function so we can defer inside the loop
			func() {
				e.state.lock.Lock()
				defer e.state.lock.Unlock()

				if err := e.handleNewConfigMap(newConf); err != nil {
					klog.Errorf("[autoscale-enforcer] Rejecting bad %s ConfigMap: %s", desc, err)
				}
			}()
		}
	}

	go listenWithName(addEvents, "new")
	go listenWithName(updateEvents, "updated")

	return nil
}

func (e *AutoscaleEnforcer) handleNewConfigMap(configMap *corev1.ConfigMap) error {
	jsonString, ok := configMap.Data[ConfigMapKey]
	if !ok {
		return fmt.Errorf("ConfigMap missing map key %q", ConfigMapKey)
	}

	// If there's an existing configuration and the JSON string is the same, then do nothing.
	if e.state.conf != nil && e.state.conf.JSONString == jsonString {
		klog.Infof("[autoscale-enforcer] Doing nothing for config update; config string unchanged")
		return nil
	}

	var newConf config
	decoder := json.NewDecoder(strings.NewReader(jsonString))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&newConf); err != nil {
		return err
	}

	if path, err := newConf.validate(); err != nil {
		return fmt.Errorf("Invalid configuration: at %s: %w", path, err)
	}

	newConf.JSONString = jsonString

	isFirstConf := e.state.conf == nil
	if !isFirstConf {
		if path, err := e.state.conf.validateChangeTo(&newConf); err != nil {
			return fmt.Errorf("Invalid configuration update: at %s: %w", path, err)
		}
	}
	e.state.conf = &newConf

	if isFirstConf {
		return nil
	}

	e.state.handleUpdatedConf()
	return nil
}

//////////////////////////////////////
// HELPER METHODS FOR USING CONFIGS //
//////////////////////////////////////

// forNode returns the individual nodeConfig for a node with a particular name, taking override
// settings into account
func (c *config) forNode(nodeName string) *nodeConfig {
	for i, set := range c.NodeOverrides {
		for _, name := range set.Nodes {
			if name == nodeName {
				return &c.NodeOverrides[i].Config
			}
		}
	}

	return &c.NodeDefaults
}

func (c *nodeConfig) vCpuLimits(total *resource.Quantity) (_ nodeResourceState[uint16], margin *resource.Quantity, _ error) {
	// We check both Value and MilliValue here in case the value overflows an int64 when
	// multiplied by 1000, which is possible if c.Cpu.System is not in units of milli-CPU
	if c.Cpu.System.Value() > total.Value() || c.Cpu.System.MilliValue() > total.MilliValue() {
		err := fmt.Errorf("desired system vCPU %v greater than node total %v", &c.Cpu.System, total)
		return nodeResourceState[uint16]{}, nil, err
	}

	totalRounded := total.MilliValue() / 1000

	// system CPU usage isn't measured directly, but as the number of additional *full* CPUs
	// reserved for system functions *that we'd otherwise have available*.
	//
	// So if c.Cpu.System is less than the difference between total.MilliValue() and
	// 1000*total.Value(), then systemCpus will be zero.
	systemCpus := totalRounded - (total.MilliValue()-c.Cpu.System.MilliValue())/1000

	reservableCpus := totalRounded - systemCpus
	unreservableCpuMillis := total.MilliValue() - 1000*reservableCpus

	margin = resource.NewMilliQuantity(unreservableCpuMillis, c.Cpu.System.Format)
	margin.Sub(c.Cpu.System)

	return nodeResourceState[uint16]{
		total:                uint16(totalRounded),
		system:               uint16(systemCpus),
		watermark:            uint16(c.Cpu.Watermark * float32(reservableCpus)),
		reserved:             0,
		capacityPressure:     0,
		pressureAccountedFor: 0,
	}, margin, nil
}

func (c *nodeConfig) memoryLimits(
	total *resource.Quantity,
	slotSize *resource.Quantity,
) (_ nodeResourceState[uint16], margin *resource.Quantity, _ error) {
	if c.Memory.System.Cmp(*total) == 1 /* if c.Memory.System > total */ {
		err := fmt.Errorf(
			"desired system memory %v greater than node total %v",
			&c.Memory.System, total,
		)
		return nodeResourceState[uint16]{}, nil, err
	} else if slotSize.Cmp(*total) == 1 /* if slotSize > total */ {
		err := fmt.Errorf("slotSize %v greater than node total %v", slotSize, total)
		return nodeResourceState[uint16]{}, nil, err
	}

	totalSlots := total.Value() / slotSize.Value()
	if totalSlots > int64(math.MaxUint16) {
		err := fmt.Errorf("too many memory slots (%d > maximum uint16)", totalSlots)
		return nodeResourceState[uint16]{}, nil, err
	}

	// systemSlots isn't measured directly, but as the number of additional slots reserved for
	// system functions *that we'd otherwise have available*.
	//
	// So if c.Memory.System is less than the leftover space between totalSlots*slotSize and total,
	// then systemSlots will be zero.
	systemSlots := totalSlots - (total.Value()-c.Memory.System.Value())/slotSize.Value()

	reservableSlots := totalSlots - systemSlots
	unreservable := total.Value() - slotSize.Value()*reservableSlots

	margin = resource.NewQuantity(unreservable, total.Format)
	margin.Sub(c.Memory.System)

	return nodeResourceState[uint16]{
		total:                uint16(totalSlots),
		system:               uint16(systemSlots),
		watermark:            uint16(c.Memory.Watermark * float32(reservableSlots)),
		reserved:             0,
		capacityPressure:     0,
		pressureAccountedFor: 0,
	}, margin, nil
}
