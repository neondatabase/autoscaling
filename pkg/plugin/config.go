package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/exp/slices"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/api"
)

//////////////////
// CONFIG TYPES //
//////////////////

type Config struct {
	// NodeConfig defines our policies around node resources and scoring
	NodeConfig nodeConfig `json:"nodeConfig"`
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

	// RandomizeScores, if true, will cause the scheduler to score a node with a random number in
	// the range [minScore + 1, trueScore], instead of the trueScore
	RandomizeScores bool `json:"randomizeScores"`

	// MigrationDeletionRetrySeconds gives the duration, in seconds, we should wait between retrying
	// a failed attempt to delete a VirtualMachineMigration that's finished.
	MigrationDeletionRetrySeconds uint `json:"migrationDeletionRetrySeconds"`

	// DoMigration, if provided, allows VM migration to be disabled
	//
	// This flag is intended to be temporary, just until NeonVM supports mgirations and we can
	// re-enable it.
	DoMigration *bool `json:"doMigration"`

	// K8sNodeGroupLabel, if provided, gives the label to use when recording k8s node groups in the
	// metrics (like for autoscaling_plugin_node_{cpu,mem}_resources_current)
	K8sNodeGroupLabel string `json:"k8sNodeGroupLabel"`

	// K8sAvailabilityZoneLabel, if provided, gives the label to use when recording nodes'
	// availability zones in the metrics (like for autoscaling_plugin_node_{cpu,mem}_resources_current)
	K8sAvailabilityZoneLabel string `json:"k8sAvailabilityZoneLabel"`

	// IgnoreNamespaces, if provided, gives a list of namespaces that the plugin should completely
	// ignore, as if pods from those namespaces do not exist.
	//
	// This is specifically designed for our "overprovisioning" namespace, which creates paused pods
	// to trigger cluster-autoscaler.
	//
	// The only exception to this rule is during Filter method calls, where we do still count the
	// resources from such pods. The reason to do that is so that these overprovisioning pods can be
	// evicted, which will allow cluster-autoscaler to trigger scale-up.
	IgnoreNamespaces []string `json:"ignoreNamespaces"`

	// DumpState, if provided, enables a server to dump internal state
	DumpState *dumpStateConfig `json:"dumpState"`

	// JSONString is the JSON string that was used to generate this config struct
	JSONString string `json:"-"`
}

type nodeConfig struct {
	Cpu         resourceConfig `json:"cpu"`
	Memory      resourceConfig `json:"memory"`
	ComputeUnit api.Resources  `json:"computeUnit"`

	// Details about node scoring:
	// See also: https://www.desmos.com/calculator/wg8s0yn63s
	// In the desmos, the value f(x,s) gives the score (from 0 to 1) of a node that's x amount full
	// (where x is a fraction from 0 to 1), with a total size that is equal to the maximum size node
	// times s (i.e. s (or: "scale") gives the ratio between this nodes's size and the biggest one).

	// MinUsageScore gives the ratio of the score at the minimum usage (i.e. 0) relative to the
	// score at the midpoint, which will have the maximum.
	//
	// This corresponds to y₀ in the desmos link above.
	MinUsageScore float64 `json:"minUsageScore"`
	// MaxUsageScore gives the ratio of the score at the maximum usage (i.e. full) relative to the
	// score at the midpoint, which will have the maximum.
	//
	// This corresponds to y₁ in the desmos link above.
	MaxUsageScore float64 `json:"maxUsageScore"`
	// ScorePeak gives the fraction at which the "target" or highest score should be, with the score
	// sloping down on either side towards MinUsageScore at 0 and MaxUsageScore at 1.
	//
	// This corresponds to xₚ in the desmos link.
	ScorePeak float64 `json:"scorePeak"`
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
}

func (c *Config) migrationEnabled() bool {
	return c.DoMigration == nil || *c.DoMigration
}

///////////////////////
// CONFIG VALIDATION //
///////////////////////

// if the returned error is not nil, the string is a JSON path to the invalid value
func (c *Config) validate() (string, error) {
	if path, err := c.NodeConfig.validate(); err != nil {
		return fmt.Sprintf("nodeConfig.%s", path), err
	}

	if c.MemSlotSize.Value() <= 0 {
		return "memBlockSize", errors.New("value must be > 0")
	} else if c.MemSlotSize.Value() <= math.MaxInt64/1000 && c.MemSlotSize.MilliValue()%1000 != 0 {
		return "memBlockSize", errors.New("value cannot have milli-precision")
	}

	if c.SchedulerName == "" {
		return "schedulerName", errors.New("string cannot be empty")
	}

	if c.DumpState != nil {
		if path, err := c.DumpState.validate(); err != nil {
			return fmt.Sprintf("dumpState.%s", path), err
		}
	}

	if c.MigrationDeletionRetrySeconds == 0 {
		return "migrationDeletionRetrySeconds", errors.New("value must be > 0")
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

	if c.MinUsageScore < 0 || c.MinUsageScore > 1 {
		return "minUsageScore", errors.New("value must be between 0 and 1, inclusive")
	} else if c.MaxUsageScore < 0 || c.MaxUsageScore > 1 {
		return "maxUsageScore", errors.New("value must be between 0 and 1, inclusive")
	} else if c.ScorePeak < 0 || c.ScorePeak > 1 {
		return "scorePeak", errors.New("value must be between 0 and 1, inclusive")
	}

	return "", nil
}

func (c *resourceConfig) validate() (string, error) {
	if c.Watermark <= 0.0 {
		return "watermark", errors.New("value must be > 0")
	} else if c.Watermark > 1.0 {
		return "watermark", errors.New("value must be <= 1")
	}

	return "", nil
}

////////////////////
// CONFIG READING //
////////////////////

const DefaultConfigPath = "/etc/scheduler-plugin-config/autoscale-enforcer-config.json"

func ReadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Error opening config file %q: %w", path, err)
	}

	defer file.Close()
	var config Config
	jsonDecoder := json.NewDecoder(file)
	jsonDecoder.DisallowUnknownFields()
	if err = jsonDecoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("Error decoding JSON config in %q: %w", path, err)
	}

	if path, err = config.validate(); err != nil {
		return nil, fmt.Errorf("Invalid config at %s: %w", path, err)
	}

	return &config, nil
}

//////////////////////////////////////
// HELPER METHODS FOR USING CONFIGS //
//////////////////////////////////////

// ignoredNamespace returns whether items in the namespace should be treated as if they don't exist
func (c *Config) ignoredNamespace(namespace string) bool {
	return slices.Contains(c.IgnoreNamespaces, namespace)
}

func (c *nodeConfig) vCpuLimits(total *resource.Quantity) (_ nodeResourceState[vmapi.MilliCPU], margin *resource.Quantity) {
	totalMilli := total.MilliValue()

	margin = resource.NewMilliQuantity(0, total.Format)

	return nodeResourceState[vmapi.MilliCPU]{
		Total:                vmapi.MilliCPU(totalMilli),
		Watermark:            vmapi.MilliCPU(c.Cpu.Watermark * float32(totalMilli)),
		Reserved:             0,
		Buffer:               0,
		CapacityPressure:     0,
		PressureAccountedFor: 0,
	}, margin
}

func (c *nodeConfig) memoryLimits(
	total *resource.Quantity,
	slotSize *resource.Quantity,
) (_ nodeResourceState[uint16], margin *resource.Quantity, _ error) {
	if slotSize.Cmp(*total) == 1 /* if slotSize > total */ {
		err := fmt.Errorf("slotSize %v greater than node total %v", slotSize, total)
		return nodeResourceState[uint16]{}, nil, err
	}

	totalSlots := total.Value() / slotSize.Value()
	if totalSlots > int64(math.MaxUint16) {
		err := fmt.Errorf("too many memory slots (%d > maximum uint16)", totalSlots)
		return nodeResourceState[uint16]{}, nil, err
	}

	unreservable := total.Value() - slotSize.Value()*totalSlots

	margin = resource.NewQuantity(unreservable, total.Format)

	return nodeResourceState[uint16]{
		Total:                uint16(totalSlots),
		Watermark:            uint16(c.Memory.Watermark * float32(totalSlots)),
		Reserved:             0,
		Buffer:               0,
		CapacityPressure:     0,
		PressureAccountedFor: 0,
	}, margin, nil
}
