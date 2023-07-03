package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/api"
)

//////////////////
// CONFIG TYPES //
//////////////////

type Config struct {
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

	// DumpState, if provided, enables a server to dump internal state
	DumpState *dumpStateConfig `json:"dumpState"`

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

func (c *Config) migrationEnabled() bool {
	return c.DoMigration == nil || *c.DoMigration
}

///////////////////////
// CONFIG VALIDATION //
///////////////////////

// if the returned error is not nil, the string is a JSON path to the invalid value
func (c *Config) validate() (string, error) {
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

	if c.DumpState != nil {
		if path, err := c.DumpState.validate(); err != nil {
			return fmt.Sprintf("dumpState.%s", path), err
		}
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

// forNode returns the individual nodeConfig for a node with a particular name, taking override
// settings into account
func (c *Config) forNode(nodeName string) *nodeConfig {
	for i, set := range c.NodeOverrides {
		for _, name := range set.Nodes {
			if name == nodeName {
				return &c.NodeOverrides[i].Config
			}
		}
	}

	return &c.NodeDefaults
}

func (c *nodeConfig) vCpuLimits(total *resource.Quantity) (_ nodeResourceState[vmapi.MilliCPU], margin *resource.Quantity, _ error) {
	// We check both Value and MilliValue here in case the value overflows an int64 when
	// multiplied by 1000, which is possible if c.Cpu.System is not in units of milli-CPU
	if c.Cpu.System.Value() > total.Value() || c.Cpu.System.MilliValue() > total.MilliValue() {
		err := fmt.Errorf("desired system vCPU %v greater than node total %v", &c.Cpu.System, total)
		return nodeResourceState[vmapi.MilliCPU]{}, nil, err
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

	return nodeResourceState[vmapi.MilliCPU]{
		Total:                vmapi.MilliCPU(totalRounded * 1000),
		System:               vmapi.MilliCPU(systemCpus * 1000),
		Watermark:            vmapi.MilliCPU(c.Cpu.Watermark * float32(reservableCpus) * 1000),
		Reserved:             0,
		Buffer:               0,
		CapacityPressure:     0,
		PressureAccountedFor: 0,
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
		Total:                uint16(totalSlots),
		System:               uint16(systemSlots),
		Watermark:            uint16(c.Memory.Watermark * float32(reservableSlots)),
		Reserved:             0,
		Buffer:               0,
		CapacityPressure:     0,
		PressureAccountedFor: 0,
	}, margin, nil
}
