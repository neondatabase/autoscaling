package util

// same as k8s.io/apimachinery/pkg/types/namespacedname.go, but with JSON (de)serialization

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FIXME: K8s uses '/' as its separator. We currently use this for backwards-compatibility, but we
// should change to '/'. That change will involve fixing the many places where we create strings
// with %s:%s instead of %s/%s as well; those should just use NamespacedName directly.
const Separator = ':'

// NamespacedName represents a resource name with the namespace it's in.
//
// When printed with '%s' or '%v', NamespacedName is rendered as "<namespace>:<name>". Printing with
// '%+v' or '%#v' renders as it would normally.
type NamespacedName struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func GetNamespacedName(obj metav1.ObjectMetaAccessor) NamespacedName {
	meta := obj.GetObjectMeta()
	return NamespacedName{Namespace: meta.GetNamespace(), Name: meta.GetName()}
}

func (n NamespacedName) Format(state fmt.State, verb rune) {
	switch {
	case verb == 'v' && state.Flag('+'):
		// Show fields, e.g. `{Namespace:foo Name:bar}`
		_, _ = state.Write([]byte(string("{Namespace:")))
		_, _ = state.Write([]byte(n.Namespace))
		_, _ = state.Write([]byte(string(" Name:")))
		_, _ = state.Write([]byte(n.Name))
		_, _ = state.Write([]byte{'}'})
	case verb == 'v' && state.Flag('#'):
		// Go syntax representation, e.g. `util.NamespacedName{Namespace:"foo", Name:"bar"}`
		_, _ = state.Write([]byte(fmt.Sprintf("util.NamespacedName{Namespace:%q, Name:%q}", n.Namespace, n.Name)))
	default:
		// Pretty-printed representation, e.g. `foo:bar`
		_, _ = state.Write([]byte(n.Namespace))
		_, _ = state.Write([]byte(string(Separator)))
		_, _ = state.Write([]byte(n.Name))
	}
}
