package plugin

import (
	"encoding/json"
	"fmt"
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

	// FallbackToAllocatable is a flag that allows the scheduler to use a node's Status.Allocatable
	// if a resource isn't present in its Status.Capacity.
	//
	// This flag *should* be false, but exists to allow a hotfix to quickly get things working if
	// the appropriate information isn't there for some reason.
	FallbackToAllocatable bool `json:"fallbackToAllocatable"`

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
	System int64 `json:"system,omitempty"`
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
		return "memBlockSize", fmt.Errorf("value must be > 0")
	}

	return "", nil
}

func (s *overrideSet) validate() (string, error) {
	if len(s.Nodes) == 0 {
		return "nodes", fmt.Errorf("array must be non-empty")
	}

	if path, err := s.Config.validate(); err != nil {
		return fmt.Sprintf("config.%s", path), err
	}

	return "", nil
}

func (c *nodeConfig) validate() (string, error) {
	if path, err := c.Cpu.validate(); err != nil {
		return fmt.Sprintf("cpu.%s", path), err
	}
	if path, err := c.Memory.validate(); err != nil {
		return fmt.Sprintf("memory.%s", path), err
	}
	if err := c.ComputeUnit.ValidateNonZero(); err != nil {
		return "computeUnit", err
	}

	return "", nil
}

func (c *resourceConfig) validate() (string, error) {
	if c.Watermark <= 0.0 {
		return "watermark", fmt.Errorf("value must be > 0")
	} else if c.Watermark > 1.0 {
		return "watermark", fmt.Errorf("value must be <= 1")
	}

	if c.System <= 0 {
		return "system", fmt.Errorf("value must be > 0")
	}

	return "", nil
}

func (oldConf *config) validateChangeTo(newConf *config) (string, error) {
	if newConf.MemSlotSize != oldConf.MemSlotSize {
		return "memSlotSize", fmt.Errorf("value cannot be changed at runtime")
	}

	return "", nil
}

/////////////////////////////////////////
// CONFIG UPDATE TRACKING AND HANDLING //
/////////////////////////////////////////

func (e *AutoscaleEnforcer) setConfigAndStartWatcher() error {
	// We want the state to be locked until the initial get request is done we make so that we don't
	// attempt to handle updates before lastConfigString has been set.
	e.state.lock.Lock()
	defer e.state.lock.Unlock()

	stop := make(chan struct{})
	addEvents := make(chan *corev1.ConfigMap)
	updateEvents := make(chan *corev1.ConfigMap)

	util.Watch(
		e.handle.ClientSet().CoreV1().RESTClient(),
		stop,
		corev1.ResourceConfigMaps,
		ConfigMapNamespace,
		util.WatchHandlerFuncs[*corev1.ConfigMap]{
			// Hooking into AddFunc as well as UpdateFunc allows for us to handle both "patch" and
			// "delete + replace" workflows for the ConfigMap.
			AddFunc:    func(newConf *corev1.ConfigMap) { addEvents <- newConf },
			UpdateFunc: func(oldConf, newConf *corev1.ConfigMap) { updateEvents <- newConf },
		},
		func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector(metav1.ObjectNameField, ConfigMapName).String()
		},
	)

	// Listen until we get one configmap.
	//
	// We have to do it this way (instead of e.g., using a List method on the returned store)
	// because using a List method is racy; it tends to use the existing stuff in the store BEFORE
	// any calls to the API server has been made, and the store starts with nothing in it.
	//
	// Unfortunately, there isn't a clean, non-racy way for us to know when the store's been updated
	// (even wrapping the REST client will still result in notification before the store is updated)
	// So we're left with a "wait to see if there's anything" + "timeout if there isn't".
	klog.Infof("Waiting for initial ConfigMap Add, timeout=%d seconds", InitConfigMapTimeoutSeconds)
	select {
	case initConfigMap := <-addEvents:
		klog.Infof("Got initial ConfigMap, data = %v", initConfigMap.Data)
		if err := e.handleNewConfigMap(initConfigMap); err != nil {
			close(stop)
			return fmt.Errorf("Bad initial ConfigMap: %w", err)
		}
	case <-time.After(time.Duration(InitConfigMapTimeoutSeconds) * time.Second):
		close(stop)
		return fmt.Errorf(
			"Timed out waiting for initial ConfigMap (expected name = %s, namespace = %s)",
			ConfigMapName, ConfigMapNamespace,
		)
	}

	// Start listening on the channels that we provided to the callbacks, now that we've gotten our
	// initial ConfigMap
	listenWithName := func(ch <-chan *corev1.ConfigMap, desc string) {
		for newConf := range ch {
			e.state.lock.Lock()
			defer e.state.lock.Unlock()

			if err := e.handleNewConfigMap(newConf); err != nil {
				klog.Errorf("[autoscale-enforcer] Rejecting bad %s ConfigMap: %w", desc, err)
			}
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
		return fmt.Errorf("Invalid configuration: at %s: %s", path, err)
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

// forNode returns the individual nodeConfig for a node with a particualr name, taking override
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

func (c *nodeConfig) vCpuLimits(total uint16) (nodeResourceState[uint16], error) {
	system := uint16(c.Cpu.System)
	if system > total {
		err := fmt.Errorf("desired system vCPU %d greater than node total %d", system, total)
		return nodeResourceState[uint16]{}, err
	}

	return nodeResourceState[uint16]{
		total:                total,
		system:               system,
		watermark:            uint16(c.Cpu.Watermark * float32(total-system)),
		reserved:             0,
		capacityPressure:     0,
		pressureAccountedFor: 0,
	}, nil
}

func (c *nodeConfig) memoryLimits(totalSlots uint16) (nodeResourceState[uint16], error) {
	system := uint16(c.Memory.System)
	if system > totalSlots {
		err := fmt.Errorf(
			"desired system memory (%d slots) greater than node total (%d slots)",
			system, totalSlots,
		)
		return nodeResourceState[uint16]{}, err
	}

	return nodeResourceState[uint16]{
		total:                totalSlots,
		system:               system,
		watermark:            uint16(c.Memory.Watermark * float32(totalSlots-system)),
		reserved:             0,
		capacityPressure:     0,
		pressureAccountedFor: 0,
	}, nil
}
