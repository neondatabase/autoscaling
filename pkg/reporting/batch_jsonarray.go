package reporting

import (
	"encoding/json"
	"fmt"
)

var _ BatchBuilder[int] = (*JSONArrayBuilder[int])(nil)

// JSONArrayBuilder is a BatchBuilder where all the events in a batch are serialized as a single
// large JSON array.
type JSONArrayBuilder[E any] struct {
	buf          IOBuffer
	started      bool
	nestingCount int
}

// NewJSONArrayBatch creates a new JSONArrayBuilder using the underlying IOBuffer to potentially
// process the JSON encoding -- either with ByteBuffer for plaintext or GZIPBuffer for gzip
// compression.
func NewJSONArrayBuilder[E any](buf IOBuffer, nestedFields ...string) *JSONArrayBuilder[E] {
	for _, fieldName := range nestedFields {
		// note: use a discrete json.Marhsal here instead of json.Encoder because encoder adds a
		// newline at the end, and that'll make the formatting weird for us.
		encodedField, err := json.Marshal(fieldName)
		if err != nil {
			panic(fmt.Sprintf("failed to JSON encode: %s", fieldName))
		}

		if _, err := buf.Write([]byte{'{'}); err != nil {
			panic(fmt.Sprintf("failed to write: %s", err))
		}
		if _, err := buf.Write(encodedField); err != nil {
			panic(fmt.Sprintf("failed to write: %s", err))
		}
		if _, err := buf.Write([]byte{':'}); err != nil {
			panic(fmt.Sprintf("failed to write: %s", err))
		}
	}
	// open the array:
	if _, err := buf.Write([]byte{'['}); err != nil {
		panic(fmt.Sprintf("failed to write: %s", err))
	}

	return &JSONArrayBuilder[E]{
		buf:          buf,
		started:      false,
		nestingCount: len(nestedFields),
	}
}

func (b *JSONArrayBuilder[E]) Add(event E) {
	if b.started {
		if _, err := b.buf.Write([]byte("\n\t,")); err != nil {
			panic(fmt.Sprintf("failed to write: %s", err))
		}
	}

	// note: we use a discrete json.Marshal here instead of json.Encoder becaues encoder adds a
	// newline at the end, and that'll make the formatting weird for us.
	tmpJSON, err := json.Marshal(event)
	if err != nil {
		panic(fmt.Sprintf("failed to JSON encode: %s", err))
	}

	if _, err := b.buf.Write(tmpJSON); err != nil {
		panic(fmt.Sprintf("failed to write: %s", err))
	}
	b.started = true
}

func (b *JSONArrayBuilder[E]) Finish() []byte {
	if _, err := b.buf.Write([]byte("\n]")); err != nil {
		panic(fmt.Sprintf("failed to write: %s", err))
	}
	for i := 0; i < b.nestingCount; i++ {
		if _, err := b.buf.Write([]byte("}")); err != nil {
			panic(fmt.Sprintf("failed to write: %s", err))
		}
	}

	return b.buf.Collect()
}
