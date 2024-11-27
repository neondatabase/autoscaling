package core

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"

	"github.com/neondatabase/autoscaling/pkg/api"
)

func Test_calculateGoalCU(t *testing.T) {
	gb := api.Bytes(1 << 30 /* 1 Gi */)
	cu := api.Resources{VCPU: 250, Mem: 1 * gb}

	defaultScalingConfig := api.ScalingConfig{
		LoadAverageFractionTarget:        lo.ToPtr(1.0),
		MemoryUsageFractionTarget:        lo.ToPtr(0.5),
		MemoryTotalFractionTarget:        lo.ToPtr(0.9),
		EnableLFCMetrics:                 lo.ToPtr(true),
		LFCToMemoryRatio:                 lo.ToPtr(0.75),
		LFCWindowSizeMinutes:             lo.ToPtr(5),
		LFCMinWaitBeforeDownscaleMinutes: lo.ToPtr(5),
		CPUStableZoneRatio:               lo.ToPtr(0.0),
		CPUMixedZoneRatio:                lo.ToPtr(0.0),
	}

	warn := func(msg string) {}

	cases := []struct {
		name       string
		cfgUpdater func(*api.ScalingConfig)
		sys        *SystemMetrics
		lfc        *LFCMetrics
		want       scalingGoal
	}{
		{
			name:       "basic",
			cfgUpdater: nil,
			sys:        nil,
			lfc:        nil,
			want: scalingGoal{
				goalCU:        0,
				hasAllMetrics: false,
			},
		},
		{
			name:       "cpu-load1-1cu",
			cfgUpdater: nil,
			//nolint:exhaustruct // this is a test
			sys: &SystemMetrics{
				LoadAverage1Min: 0.2,
			},
			lfc: nil,
			want: scalingGoal{
				goalCU:        1,
				hasAllMetrics: false,
			},
		},
		{
			name:       "cpu-load1-4cu",
			cfgUpdater: nil,
			//nolint:exhaustruct // this is a test
			sys: &SystemMetrics{
				LoadAverage1Min: 1,
			},
			lfc: nil,
			want: scalingGoal{
				goalCU:        4,
				hasAllMetrics: false,
			},
		},
		{
			name: "cpu-zone-load1",
			cfgUpdater: func(cfg *api.ScalingConfig) {
				cfg.CPUStableZoneRatio = lo.ToPtr(0.5)
			},
			//nolint:exhaustruct // this is a test
			sys: &SystemMetrics{
				LoadAverage1Min: 0.7, // equal to 3 CUs
				LoadAverage5Min: 0.0,
			},
			lfc: nil,
			want: scalingGoal{
				goalCU:        3,
				hasAllMetrics: false,
			},
		},
		{
			name: "cpu-zone-load5",
			cfgUpdater: func(cfg *api.ScalingConfig) {
				cfg.CPUStableZoneRatio = lo.ToPtr(0.5)
			},
			sys: &SystemMetrics{
				LoadAverage1Min:   1,   // value is ignored, because it is in the stable zone
				LoadAverage5Min:   0.7, // equal to 3 CUs
				MemoryUsageBytes:  0,
				MemoryCachedBytes: 0,
			},
			lfc: nil,
			want: scalingGoal{
				goalCU:        3,
				hasAllMetrics: false,
			},
		},
		{
			name: "cpu-zone-mixed",
			cfgUpdater: func(cfg *api.ScalingConfig) {
				cfg.CPUStableZoneRatio = lo.ToPtr(0.5)
				cfg.CPUMixedZoneRatio = lo.ToPtr(0.5)
			},
			sys: &SystemMetrics{
				LoadAverage1Min:   1.75, // 1.75*4 = 7 CUs
				LoadAverage5Min:   1,    // 1*4 = 4 CUs
				MemoryUsageBytes:  0,
				MemoryCachedBytes: 0,
			},
			lfc: nil,
			want: scalingGoal{
				goalCU:        5, // Weighted average between 7 and 4 CUs
				hasAllMetrics: false,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scalingConfig := defaultScalingConfig
			if c.cfgUpdater != nil {
				c.cfgUpdater(&scalingConfig)
			}

			got, _ := calculateGoalCU(warn, scalingConfig, cu, c.sys, c.lfc)
			assert.Equal(t, c.want, got)
		})
	}
}
