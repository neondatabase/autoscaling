package testhelpers

import (
	"testing"

	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type InitialVmInfoConfig struct {
	ComputeUnit    api.Resources
	MemorySlotSize resource.Quantity

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

type InitialVmInfoOpt interface {
	InitialStateOpt

	modifyVmInfoConfig(*InitialVmInfoConfig)
	modifyVmInfoWithConfig(InitialVmInfoConfig, *api.VmInfo)
}

func CreateInitialState(config InitialStateConfig, opts ...InitialStateOpt) *core.State {
	vmOpts := []InitialVmInfoOpt{}
	for _, o := range opts {
		if vo, ok := o.(InitialVmInfoOpt); ok {
			vmOpts = append(vmOpts, vo)
		}
	}

	vm := CreateInitialVmInfo(config.VM, vmOpts...)

	for _, o := range opts {
		o.modifyStateConfig(&config.Core)
	}

	return core.NewState(vm, config.Core)
}

func CreateInitialVmInfo(config InitialVmInfoConfig, opts ...InitialVmInfoOpt) api.VmInfo {
	for _, o := range opts {
		o.modifyVmInfoConfig(&config)
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
			SlotSize: &config.MemorySlotSize,
			Min:      config.MinCU * config.ComputeUnit.Mem,
			Use:      config.MinCU * config.ComputeUnit.Mem,
			Max:      config.MaxCU * config.ComputeUnit.Mem,
		},
		ScalingConfig:  nil,
		AlwaysMigrate:  false,
		ScalingEnabled: true,
	}

	for _, o := range opts {
		o.modifyVmInfoWithConfig(config, &vm)
	}

	return vm
}

type coreConfigModifier func(*core.Config)
type vmInfoConfigModifier func(*InitialVmInfoConfig)
type vmInfoModifier func(InitialVmInfoConfig, *api.VmInfo)

var (
	_ InitialVmInfoOpt = vmInfoConfigModifier(nil)
	_ InitialVmInfoOpt = vmInfoModifier(nil)
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

func WithMinMaxCU(minCU, maxCU uint16) InitialStateOpt {
	return vmInfoConfigModifier(func(c *InitialVmInfoConfig) {
		c.MinCU = minCU
		c.MaxCU = maxCU
	})
}

func WithInitialCU(cu uint16) InitialStateOpt {
	return vmInfoModifier(func(c InitialVmInfoConfig, vm *api.VmInfo) {
		vm.SetUsing(c.ComputeUnit.Mul(cu))
	})
}
