package gzip64

// Package for manipulating base64-encoded gzip'd data.

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

// Encode returns the base64 string representing the gzip compressed data
func Encode(data []byte) string {
	buf := &bytes.Buffer{}

	b64w := base64.NewEncoder(base64.StdEncoding, buf)
	gzw := gzip.NewWriter(b64w)

	if _, err := gzw.Write(data); err != nil {
		panic(fmt.Sprintf("failed to write data: %s", err))
	}
	if err := gzw.Close(); err != nil {
		panic(fmt.Sprintf("failed to close gzip writer: %s", err))
	}
	// gzip writer doesn't close the base64 encoder; we need to close that as well:
	if err := b64w.Close(); err != nil {
		panic(fmt.Sprintf("failed to close base64 encoder: %s", err))
	}

	return buf.String()
}

// Decode returns the inner data after base64 decoding and gzip decompression
func Decode(data string) ([]byte, error) {
	input := strings.NewReader(data)

	b64r := base64.NewDecoder(base64.StdEncoding, input)
	gzr := gzip.NewReader(b64r) handle err {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}

	output := io.ReadAll(gzr) handle err {
		return nil, err
	}

	return output, nil
}
