package testhelpers

import (
	"fmt"
	"testing"

	"go.uber.org/zap"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/agent/core/logiclock"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type InitialVmInfoConfig struct {
	ComputeUnit    api.Resources
	MemorySlotSize api.Bytes

	MinCU uint16
	MaxCU uint16
}

type InitialStateConfig struct {
	VM InitialVmInfoConfig

	Core core.Config
}

type InitialStateOpt interface {
	modifyStateConfig(*core.Config)
}

type VmInfoOpt interface {
	InitialStateOpt

	modifyVmInfoConfig(*InitialVmInfoConfig)
	modifyVmInfoWithConfig(InitialVmInfoConfig, *api.VmInfo)
}

type ClockSourceOpt interface {
	InitialStateOpt

	clock() core.LogicClock
}

func CreateInitialState(config InitialStateConfig, opts ...InitialStateOpt) *core.State {
	vmOpts := []VmInfoOpt{}
	var clock core.LogicClock

	clock = &logiclock.NilClock{}
	for _, o := range opts {
		if vo, ok := o.(VmInfoOpt); ok {
			vmOpts = append(vmOpts, vo)
		}
		if co, ok := o.(ClockSourceOpt); ok {
			clock = co.clock()
		}
	}

	vm := CreateVmInfo(config.VM, vmOpts...)

	for _, o := range opts {
		o.modifyStateConfig(&config.Core)
	}

	return core.NewState(vm, config.Core, clock)
}

func CreateVmInfo(config InitialVmInfoConfig, opts ...VmInfoOpt) api.VmInfo {
	for _, o := range opts {
		o.modifyVmInfoConfig(&config)
	}

	if config.ComputeUnit.Mem%config.MemorySlotSize != 0 {
		panic(fmt.Errorf(
			"compute unit is not divisible by memory slot size: %v is not divisible by %v",
			config.ComputeUnit.Mem,
			config.MemorySlotSize,
		))
	}

	vm := api.VmInfo{
		Name:      "test",
		Namespace: "test",
		Cpu: api.VmCpuInfo{
			Min: vmapi.MilliCPU(config.MinCU) * config.ComputeUnit.VCPU,
			Use: vmapi.MilliCPU(config.MinCU) * config.ComputeUnit.VCPU,
			Max: vmapi.MilliCPU(config.MaxCU) * config.ComputeUnit.VCPU,
		},
		Mem: api.VmMemInfo{
			SlotSize: config.MemorySlotSize,
			Min:      config.MinCU * uint16(config.ComputeUnit.Mem/config.MemorySlotSize),
			Use:      config.MinCU * uint16(config.ComputeUnit.Mem/config.MemorySlotSize),
			Max:      config.MaxCU * uint16(config.ComputeUnit.Mem/config.MemorySlotSize),
		},
		Config: api.VmConfig{
			AutoMigrationEnabled: false,
			AlwaysMigrate:        false,
			ScalingConfig:        nil,
			ScalingEnabled:       true,
		},
		CurrentLogicalTime: nil,
	}

	for _, o := range opts {
		o.modifyVmInfoWithConfig(config, &vm)
	}

	return vm
}

type coreConfigModifier func(*core.Config)
type vmInfoConfigModifier func(*InitialVmInfoConfig)
type vmInfoModifier func(InitialVmInfoConfig, *api.VmInfo)
type clockInjector func() core.LogicClock

var (
	_ VmInfoOpt = vmInfoConfigModifier(nil)
	_ VmInfoOpt = vmInfoModifier(nil)
)

func (m coreConfigModifier) modifyStateConfig(c *core.Config) { (func(*core.Config))(m)(c) }
func (m vmInfoConfigModifier) modifyStateConfig(*core.Config) {}
func (m vmInfoModifier) modifyStateConfig(*core.Config)       {}

func (m vmInfoModifier) modifyVmInfoConfig(*InitialVmInfoConfig) {}
func (m vmInfoConfigModifier) modifyVmInfoConfig(c *InitialVmInfoConfig) {
	(func(*InitialVmInfoConfig))(m)(c)
}

func (m vmInfoConfigModifier) modifyVmInfoWithConfig(InitialVmInfoConfig, *api.VmInfo) {}
func (m vmInfoModifier) modifyVmInfoWithConfig(c InitialVmInfoConfig, vm *api.VmInfo) {
	(func(InitialVmInfoConfig, *api.VmInfo))(m)(c, vm)
}

func (m clockInjector) modifyStateConfig(*core.Config) {}
func (m clockInjector) clock() core.LogicClock {
	return m()
}

func WithConfigSetting(f func(*core.Config)) InitialStateOpt {
	return coreConfigModifier(f)
}

func WithStoredWarnings(warnings *[]string) InitialStateOpt {
	return WithConfigSetting(func(c *core.Config) {
		warn := c.Log.Warn
		c.Log.Warn = func(msg string, fields ...zap.Field) {
			*warnings = append(*warnings, msg)
			if warn != nil {
				warn(msg, fields...)
			}
		}
	})
}

func WithTestingLogfWarnings(t *testing.T) InitialStateOpt {
	return WithConfigSetting(func(c *core.Config) {
		warn := c.Log.Warn
		c.Log.Warn = func(msg string, fields ...zap.Field) {
			t.Log(msg)
			if warn != nil {
				warn(msg, fields...)
			}
		}
	})
}

func WithMinMaxCU(minCU, maxCU uint16) VmInfoOpt {
	return vmInfoConfigModifier(func(c *InitialVmInfoConfig) {
		c.MinCU = minCU
		c.MaxCU = maxCU
	})
}

func WithCurrentCU(cu uint16) VmInfoOpt {
	return vmInfoModifier(func(c InitialVmInfoConfig, vm *api.VmInfo) {
		vm.SetUsing(c.ComputeUnit.Mul(cu))
	})
}

func WithLogicalTime(t *vmapi.LogicalTime) VmInfoOpt {
	return vmInfoModifier(func(c InitialVmInfoConfig, vm *api.VmInfo) {
		vm.CurrentLogicalTime = t
	})
}

func WithClock(c core.LogicClock) ClockSourceOpt {
	return clockInjector(func() core.LogicClock {
		return c
	})
}
