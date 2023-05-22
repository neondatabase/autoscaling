package util

// Utilities for errors

import (
	"errors"
)

// RootError returns the root cause of the error, calling errors.Unwrap until it returns nil
func RootError(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
