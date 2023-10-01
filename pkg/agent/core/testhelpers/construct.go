package testhelpers

import (
	"fmt"

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
	postCreate func(*api.VmInfo)
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
			o.postCreate(&vm)
		}
	}

	return core.NewState(vm, config.Core)
}

func WithStoredWarnings(warnings *[]string) InitialStateOpt {
	return InitialStateOpt{
		postCreate: nil,
		preCreate: func(c *InitialStateConfig) {
			c.Core.Warn = func(format string, args ...any) {
				*warnings = append(*warnings, fmt.Sprintf(format, args...))
			}
		},
	}
}
