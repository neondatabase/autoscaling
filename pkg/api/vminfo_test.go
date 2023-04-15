package api_test

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/api"
)

func TestFormatting(t *testing.T) {
	slotSize := resource.MustParse("1Gi")
	base := any(api.VmInfo{
		Name:      "foo",
		Namespace: "bar",
		Cpu: api.VmCpuInfo{
			Min: 1,
			Max: 5,
			Use: 3,
		},
		Mem: api.VmMemInfo{
			Min:      2,
			Max:      6,
			Use:      4,
			SlotSize: &slotSize,
		},
		AlwaysMigrate:  false,
		ScalingEnabled: true,
	})
	defaultFormat := "{Name:foo Namespace:bar Cpu:{Min:1 Max:5 Use:3} Mem:{Min:2 Max:6 Use:4 SlotSize:1Gi} AlwaysMigrate:false ScalingEnabled:true}"
	goSyntaxRepr := `api.VmInfo{Name:"foo", Namespace:"bar", Cpu:api.VmCpuInfo{Min:1, Max:5, Use:3}, Mem:api.VmMemInfo{Min:2, Max:6, Use:4, SlotSize:&resource.Quantity{i:resource.int64Amount{value:1073741824, scale:0}, d:resource.infDecAmount{Dec:(*inf.Dec)(nil)}, s:"1Gi", Format:"BinarySI"}}, AlwaysMigrate:false, ScalingEnabled:true}`
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
