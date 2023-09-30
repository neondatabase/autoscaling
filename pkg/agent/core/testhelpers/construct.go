package testhelpers

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"

	"github.com/neondatabase/autoscaling/pkg/agent/core"
	"github.com/neondatabase/autoscaling/pkg/api"
)

var DefaultConfig = core.Config{
	DefaultScalingConfig: api.ScalingConfig{
		LoadAverageFractionTarget: 0.5,
		MemoryUsageFractionTarget: 0.5,
	},
	PluginRequestTick:              5 * time.Second,
	PluginDeniedRetryWait:          2 * time.Second,
	MonitorDeniedDownscaleCooldown: 5 * time.Second,
	MonitorRetryWait:               3 * time.Second,
	Warn:                           func(string, ...any) {},
}

type InitialStateOpt struct {
	preCreate  func(*initialStateParams)
	postCreate func(*api.VmInfo, *core.Config)
}

type initialStateParams struct {
	computeUnit api.Resources
	minCU       uint16
	maxCU       uint16
}

func CreateInitialState(opts ...InitialStateOpt) *core.State {
	pre := initialStateParams{
		computeUnit: api.Resources{VCPU: 250, Mem: 1},
		minCU:       1,
		maxCU:       4,
	}
	for _, o := range opts {
		if o.preCreate != nil {
			o.preCreate(&pre)
		}
	}

	vm := api.VmInfo{
		Name:      "test",
		Namespace: "test",
		Cpu: api.VmCpuInfo{
			Min: vmapi.MilliCPU(pre.minCU) * pre.computeUnit.VCPU,
			Use: vmapi.MilliCPU(pre.minCU) * pre.computeUnit.VCPU,
			Max: vmapi.MilliCPU(pre.maxCU) * pre.computeUnit.VCPU,
		},
		Mem: api.VmMemInfo{
			SlotSize: resource.NewQuantity(1<<30 /* 1 Gi */, resource.BinarySI),
			Min:      pre.minCU * pre.computeUnit.Mem,
			Use:      pre.minCU * pre.computeUnit.Mem,
			Max:      pre.maxCU * pre.computeUnit.Mem,
		},
		ScalingConfig:  nil,
		AlwaysMigrate:  false,
		ScalingEnabled: true,
	}

	config := core.Config{
		DefaultScalingConfig: api.ScalingConfig{
			LoadAverageFractionTarget: 0.5,
			MemoryUsageFractionTarget: 0.5,
		},
		PluginRequestTick:              5 * time.Second,
		PluginDeniedRetryWait:          2 * time.Second,
		MonitorDeniedDownscaleCooldown: 5 * time.Second,
		MonitorRetryWait:               3 * time.Second,
		Warn:                           func(string, ...any) {},
	}

	for _, o := range opts {
		if o.postCreate != nil {
			o.postCreate(&vm, &config)
		}
	}

	return core.NewState(vm, config)
}

func WithStoredWarnings(warnings *[]string) InitialStateOpt {
	return InitialStateOpt{
		preCreate: nil,
		postCreate: func(_ *api.VmInfo, config *core.Config) {
			config.Warn = func(format string, args ...any) {
				*warnings = append(*warnings, fmt.Sprintf(format, args...))
			}
		},
	}
}
