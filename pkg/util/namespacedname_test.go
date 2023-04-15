package util_test

import (
	"fmt"
	"testing"

	"github.com/neondatabase/autoscaling/pkg/util"
)

func TestFormatting(t *testing.T) {
	base := util.NamespacedName{Namespace: "foo", Name: "bar"}
	cases := []struct {
		name     string
		expected string
		got      string
	}{
		{"sprint", "foo/bar", fmt.Sprint(base)},
		{"sprintf-%s", "foo/bar", fmt.Sprintf("%s", base)},
		{"sprintf-%v", "foo/bar", fmt.Sprintf("%v", base)},
		{"sprintf-%+v", `{Namespace:foo Name:bar}`, fmt.Sprintf("%+v", base)},
		{"sprintf-%#v", `util.NamespacedName{Namespace:"foo", Name:"bar"}`, fmt.Sprintf("%#v", base)},
	}

	for _, c := range cases {
		if c.got != c.expected {
			t.Errorf("%s: expected %q but got %q", c.name, c.expected, c.got)
		}
	}
}
