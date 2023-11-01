package executor

// Generation numbers, for use by implementers of the various interfaces (i.e. pkg/agent/execbridge.go)

import (
	"sync/atomic"
)

type StoredGenerationNumber struct {
	value atomic.Int64
}

type GenerationNumber struct {
	value int64
}

func NewStoredGenerationNumber() *StoredGenerationNumber {
	return &StoredGenerationNumber{value: atomic.Int64{}}
}

// Inc increments the stored GenerationNumber, returning the new value
func (n *StoredGenerationNumber) Inc() GenerationNumber {
	return GenerationNumber{value: n.value.Add(1)}
}

// Get fetches the current value of the stored GenerationNumber
func (n *StoredGenerationNumber) Get() GenerationNumber {
	return GenerationNumber{value: n.value.Load()}
}
