package reporting

import (
	"encoding/json"
	"fmt"
)

var _ BatchBuilder[int] = (*JSONLinesBuilder[int])(nil)

// JSONLinesBuilder is a BatchBuilder where each event in the batch is serialized as a separate JSON
// object on its own line, adhering to the "JSON lines"/"jsonl" format.
type JSONLinesBuilder[E any] struct {
	buf IOBuffer
}

func NewJSONLinesBuilder[E any](buf IOBuffer) *JSONLinesBuilder[E] {
	return &JSONLinesBuilder[E]{
		buf: buf,
	}
}

func (b *JSONLinesBuilder[E]) Add(event E) {
	tmpJSON, err := json.Marshal(event)
	if err != nil {
		panic(fmt.Sprintf("failed to JSON encode: %s", err))
	}

	if _, err := b.buf.Write(tmpJSON); err != nil {
		panic(fmt.Sprintf("failed to write: %s", err))
	}
	if _, err := b.buf.Write([]byte{'\n'}); err != nil {
		panic(fmt.Sprintf("failed to write: %s", err))
	}
}

func (b *JSONLinesBuilder[E]) Finish() []byte {
	return b.buf.Collect()
}
