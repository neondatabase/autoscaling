package api_test

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	vmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
)

func TestFormatting(t *testing.T) {
	slotSize := resource.MustParse("1Gi")
	base := any(api.VmInfo{
		Name:      "foo",
		Namespace: "bar",
		Cpu: api.VmCpuInfo{
			Min: vmapi.MilliCPU(1000),
			Max: vmapi.MilliCPU(5000),
			Use: vmapi.MilliCPU(3750),
		},
		Mem: api.VmMemInfo{
			Min:      2,
			Max:      6,
			Use:      4,
			SlotSize: api.BytesFromResourceQuantity(slotSize),
		},
		Config: api.VmConfig{
			AutoMigrationEnabled: true,
			AlwaysMigrate:        false,
			ScalingEnabled:       true,
			ScalingConfig: &api.ScalingConfig{
				LoadAverageFractionTarget: 0.7,
				MemoryUsageFractionTarget: 0.7,
			},
		},
	})
	defaultFormat := "{Name:foo Namespace:bar Cpu:{Min:1 Max:5 Use:3.75} Mem:{Min:2 Max:6 Use:4 SlotSize:1Gi} Config:{AutoMigrationEnabled:true AlwaysMigrate:false ScalingEnabled:true ScalingConfig:&{LoadAverageFractionTarget:0.7 MemoryUsageFractionTarget:0.7}}}"
	goSyntaxRepr := `api.VmInfo{Name:"foo", Namespace:"bar", Cpu:api.VmCpuInfo{Min:api.MilliCPU(1000), Max:api.MilliCPU(5000), Use:api.MilliCPU(3750)}, Mem:api.VmMemInfo{Min:2, Max:6, Use:4, SlotSize:1073741824}, Config:api.VmConfig{AutoMigrationEnabled:true, AlwaysMigrate:false, ScalingEnabled:true, ScalingConfig:&api.ScalingConfig{LoadAverageFractionTarget:0.7, MemoryUsageFractionTarget:0.7}}}`
	cases := []struct {
		name     string
		expected string
		got      string
	}{
		{"sprint", defaultFormat, fmt.Sprint(base)},
		{"sprintf-%v", defaultFormat, fmt.Sprintf("%v", base)},
		{"sprintf-%+v", defaultFormat, fmt.Sprintf("%+v", base)},
		{"sprintf-%#v", goSyntaxRepr, fmt.Sprintf("%#v", base)},
		{"sprintf-%q", fmt.Sprintf("%%!q(api.VmInfo=%s)", defaultFormat), fmt.Sprintf("%q", base)},
		//                          ^^ actually '%!q(api.VmInfo=...)'
	}

	for _, c := range cases {
		if c.got != c.expected {
			t.Errorf("%s: expected %q but got %q", c.name, c.expected, c.got)
		}
	}
}
