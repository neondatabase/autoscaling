package reporting

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

var (
	_ IOBuffer = ByteBuffer{} //nolint:exhaustruct // just for typechecking
	_ IOBuffer = GZIPBuffer{} //nolint:exhaustruct // just for typechecking
)

type IOBuffer interface {
	io.Writer
	Collect() []byte
}

// ByteBuffer is an IOBuffer that does nothing special, just wrapping a bytes.Buffer to return the
// bytes when done.
type ByteBuffer struct {
	buf *bytes.Buffer
}

func NewByteBuffer() ByteBuffer {
	return ByteBuffer{
		buf: &bytes.Buffer{},
	}
}

func (b ByteBuffer) Write(bytes []byte) (int, error) {
	return b.buf.Write(bytes)
}

func (b ByteBuffer) Collect() []byte {
	return b.buf.Bytes()
}

// WithByteBuffer is a convenience function to produce a generator for a type that requires an
// IOBuffer as input by providing a ByteBuffer.
//
// For example, this can be used with something like NewJSONLinesBuilder to create a BatchBuilder
// generator, e.g. WithByteBuffer(NewJSONLinesBuilder).
func WithByteBuffer[T any](mk func(IOBuffer) T) func() T {
	return func() T {
		return mk(NewByteBuffer())
	}
}

// GZIPBuffer is an IOBuffer that GZIP compresses all data that's written to it.
type GZIPBuffer struct {
	buf *bytes.Buffer
	gz  *gzip.Writer
}

func NewGZIPBuffer() GZIPBuffer {
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)

	return GZIPBuffer{
		buf: buf,
		gz:  gz,
	}
}

func (b GZIPBuffer) Write(bytes []byte) (int, error) {
	return b.gz.Write(bytes)
}

func (b GZIPBuffer) Collect() []byte {
	if err := b.gz.Close(); err != nil {
		panic(fmt.Sprintf("unexpected gzip error: %s", err))
	}

	return b.buf.Bytes()
}

// WithGZIPBuffer is a convenience function to produce a generator for a type that requires an
// IOBuffer as input by providing a GZIPBuffer.
//
// For example, this can be used with something like NewJSONLinesBuilder to create a BatchBuilder
// generator, e.g. WithGZIPBuffer(NewJSONLinesBuilder).
func WithGZIPBuffer[T any](mk func(IOBuffer) T) func() T {
	return func() T {
		return mk(NewGZIPBuffer())
	}
}
