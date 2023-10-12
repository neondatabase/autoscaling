// Construction of JSON patches. See https://jsonpatch.com/

package patch

import (
	"strings"
)

// OpKind is the kind of operation being performed in a single step
type OpKind string

const (
	OpAdd     OpKind = "add"
	OpRemove  OpKind = "remove"
	OpReplace OpKind = "replace"
	OpMove    OpKind = "move"
	OpCopy    OpKind = "copy"
	OpTest    OpKind = "test"
)

type JSONPatch = []Operation

// Operation is a single step in the overall JSON patch
type Operation struct {
	// Op is the kind of operation being performed in this step. See [OpKind] for more.
	Op OpKind `json:"op"`
	// Path is a [JSON pointer] to the target location of the operation.
	//
	// In general, nesting is separated by '/'s, with special characters escaped by '~'.
	// [PathEscape] is provided to handle escaping, because it can get a little gnarly.
	//
	// As an example, if you want to add a field "foo" to the first element of an array,
	// you'd use the path `/0/foo`. The jsonpatch website has more details (and clearer examples),
	// refer there for more information: https://jsonpatch.com/#json-pointer
	//
	// [JSON pointer]: https://datatracker.ietf.org/doc/html/rfc6901/
	Path string `json:"path"`
	// From gives the source location for "copy" or "move" operations.
	From string `json:"from,omitempty"`
	// Value is the new value to use, for "add", "replace", or "test" operations.
	Value any `json:"value,omitempty"`
}

var pathEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// PathEscape escapes a string for use in a segment of the Path field of an Operation
//
// This is useful, for example, when using arbitrary strings as map keys (like Kubernetes labels or
// annotations).
func PathEscape(s string) string {
	return pathEscaper.Replace(s)
}
