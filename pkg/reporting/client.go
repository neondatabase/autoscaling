package reporting

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
)

type Client[E any] struct {
	Name            string
	Base            BaseClient
	BaseConfig      BaseClientConfig
	GenerateTraceID func() string
	SerializeBatch  func(events []E) ([]byte, SimplifiableError)
}

// BaseClient is the shared lower-level interface to send the processed data somewhere.
//
// See S3Client, AzureBlobClient, and HTTPClient.
type BaseClient interface {
	LogFields() zap.Field
	Send(ctx context.Context, payload []byte, traceID string) SimplifiableError
}

type BaseClientConfig struct {
	PushEverySeconds          uint `json:"pushEverySeconds"`
	PushRequestTimeoutSeconds uint `json:"pushRequestTimeoutSeconds"`
	MaxBatchSize              uint `json:"maxBatchSize"`
}

// SimplifiableError is an extension of the standard 'error' interface that provides a
// safe-for-metrics string representing the error.
type SimplifiableError interface {
	error

	Simplified() string
}

// WrapSerialize is a combinator that takes an existing function valid for Client.SerializeBatch and
// produces a new function that applies the 'wrapper' function to the output before returning it.
//
// This can be used, for example, to provide a SerializeBatch implementation that gzips the data
// after encoding it as JSON, e.g., by:
//
//	WrapSerialize(GZIPCompress, JSONLinesMarshalBatch)
func WrapSerialize[E any](
	wrapper func([]byte) ([]byte, SimplifiableError),
	base func([]E) ([]byte, SimplifiableError),
) func([]E) ([]byte, SimplifiableError) {
	return func(events []E) ([]byte, SimplifiableError) {
		bs, err := base(events)
		if err != nil {
			return nil, err
		}
		return wrapper(bs)
	}
}

// JSONMarshalBatch is a helper function to trivially build a function satisfying
// Client.SerializeBatch.
//
// This function can't *directly* be used, because it takes any type as input, but a small wrapper
// function typically will suffice.
//
// Why not take a list directly? Sometimes there's a small amount of wrapping we'd like to do, e.g.
// packaging it as a struct instead of directly an array.
//
// See also: JSONLinesMarshalBatch, which *can* be used directly.
func JSONMarshalBatch[V any](value V) ([]byte, SimplifiableError) {
	bs, err := json.Marshal(value)
	if err != nil {
		return nil, jsonError{err: err}
	}
	return bs, nil
}

// JSONLinesMarshalBatch is a function to implement Client.SerializeBatch by serializing each event
// in the batch onto a separate JSON line.
//
// See also: JSONMarshalBatch
func JSONLinesMarshalBatch[E any](events []E) ([]byte, SimplifiableError) {
	buf := bytes.Buffer{}
	encoder := json.NewEncoder(&buf)
	for i := range events {
		// note: encoder.Encode appends a newline after encoding. This makes it valid for the
		// "json lines" format.
		err := encoder.Encode(&events[i])
		if err != nil {
			return nil, jsonError{err: err}
		}
	}
	return buf.Bytes(), nil
}

type jsonError struct {
	err error
}

func (e jsonError) Error() string {
	return fmt.Sprintf("%s: %s", e.Simplified(), e.err.Error())
}

func (e jsonError) Unwrap() error {
	return e.err
}

func (e jsonError) Simplified() string {
	return "JSON marshaling error"
}

// GZIPCompress is a helper function to compress a byte string with gzip
func GZIPCompress(payload []byte) ([]byte, SimplifiableError) {
	buf := bytes.Buffer{}

	gzW := gzip.NewWriter(&buf)
	_, err := gzW.Write(payload)
	if err != nil {
		return nil, gzipError{err: err}
	}

	err = gzW.Close() // Have to close it before reading the buffer
	if err != nil {
		return nil, gzipError{err: err}
	}
	return buf.Bytes(), nil
}

type gzipError struct {
	err error
}

func (e gzipError) Error() string {
	return fmt.Sprintf("%s: %s", e.Simplified(), e.err.Error())
}

func (e gzipError) Unwrap() error {
	return e.err
}

func (e gzipError) Simplified() string {
	return "gzip compression error"
}
