package testhelpers

import (
	"testing"

	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

type InitialStateConfig struct {
	ComputeUnit    api.Resources
	MemorySlotSize resource.Quantity

	MinCU uint16
	MaxCU uint16

	Core core.Config
}

type InitialStateOpt struct {
	preCreate  func(*InitialStateConfig)
	postCreate func(InitialStateConfig, *api.VmInfo)
}

func CreateInitialState(config InitialStateConfig, opts ...InitialStateOpt) *core.State {
	for _, o := range opts {
		if o.preCreate != nil {
			o.preCreate(&config)
		}
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
		if o.postCreate != nil {
			o.postCreate(config, &vm)
		}
	}

	return core.NewState(vm, config.Core)
}

func WithStoredWarnings(warnings *[]string) InitialStateOpt {
	return InitialStateOpt{
		postCreate: nil,
		preCreate: func(c *InitialStateConfig) {
			warn := c.Core.Log.Warn
			c.Core.Log.Warn = func(msg string, fields ...zap.Field) {
				*warnings = append(*warnings, msg)
				if warn != nil {
					warn(msg, fields...)
				}
			}
		},
	}
}

func WithTestingLogfWarnings(t *testing.T) InitialStateOpt {
	return InitialStateOpt{
		postCreate: nil,
		preCreate: func(c *InitialStateConfig) {
			warn := c.Core.Log.Warn
			c.Core.Log.Warn = func(msg string, fields ...zap.Field) {
				t.Log(msg)
				if warn != nil {
					warn(msg, fields...)
				}
			}
		},
	}
}

func WithMinMaxCU(minCU, maxCU uint16) InitialStateOpt {
	return InitialStateOpt{
		preCreate: func(c *InitialStateConfig) {
			c.MinCU = minCU
			c.MaxCU = maxCU
		},
		postCreate: nil,
	}
}

func WithInitialCU(cu uint16) InitialStateOpt {
	return InitialStateOpt{
		preCreate: nil,
		postCreate: func(c InitialStateConfig, vm *api.VmInfo) {
			vm.SetUsing(c.ComputeUnit.Mul(cu))
		},
	}
}

func WithConfigSetting(f func(*core.Config)) InitialStateOpt {
	return InitialStateOpt{
		preCreate: func(c *InitialStateConfig) {
			f(&c.Core)
		},
		postCreate: nil,
	}
}
