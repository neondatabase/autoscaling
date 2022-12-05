// Construction of JSON patch messages. See https://jsonpatch.com/

package util

import (
	"strings"
)

type JSONPatchOp string

const (
	PatchAdd     JSONPatchOp = "add"
	PatchRemove  JSONPatchOp = "remove"
	PatchReplace JSONPatchOp = "replace"
	PatchMove    JSONPatchOp = "move"
	PatchCopy    JSONPatchOp = "copy"
	PatchTest    JSONPatchOp = "test"
)

type JSONPatch struct {
	Op    JSONPatchOp `json:"op"`
	From  string      `json:"from,omitempty"`
	Path  string      `json:"path"`
	Value any         `json:"value,omitempty"`
}

var pathEscaper = strings.NewReplacer("~", "~0", "/", "~1")

func PatchPathEscape(path string) string {
	return pathEscaper.Replace(path)
}
