package testhelpers

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
)

type Assert struct {
	t                     *testing.T
	storedWarnings        *[]string
	waitingOnPreparedCall *bool

	tinfo transactionInfo
}

type transactionInfo struct {
	expectedWarnings []string
}

// NewAssert creates a new Assert object wrapping the provided *testing.T
func NewAssert(t *testing.T) Assert {
	return Assert{
		t:                     t,
		storedWarnings:        &[]string{},
		waitingOnPreparedCall: lo.ToPtr(false),
		tinfo: transactionInfo{
			expectedWarnings: []string{},
		},
	}
}

// StoredWarnings returns a reference to the warnings that will be checked, intended to be used with
// the InitialStateOpt constructor WithStoredWarnings
func (a Assert) StoredWarnings() *[]string {
	return a.storedWarnings
}

// WithWarnings returns an Assert that expects the given warnings to be emitted on each operation
func (a Assert) WithWarnings(warnings ...string) Assert {
	a.tinfo.expectedWarnings = warnings
	return a
}

// Do calls the function with the provided arguments, checking that no unexpected warnings were
// generated
//
// This is only valid for functions that return nothing.
func (a Assert) Do(f any, args ...any) {
	a.Call(f, args...).Equals( /* empty args list means no returns */ )
}

// NoError calls the function with the provided arguments, checking that the error it returns is
// nil, and that no unexpected warnings were generated.
func (a Assert) NoError(f any, args ...any) {
	a.Call(f, args...).Equals(nil)
}

// Call sets up a prepared function call, which will not be executed until one of its methods is
// actually called, which will perform all the relevant checks.
//
// Variadic functions are not supported.
func (a Assert) Call(f any, args ...any) PreparedFunctionCall {
	if *a.waitingOnPreparedCall {
		panic(errors.New("previous Call() constructed but not executed (must use `Do()`, `NoError()`, or `Call().Equals()`)"))
	}

	fv := reflect.ValueOf(f)
	fTy := fv.Type()
	if fTy.Kind() != reflect.Func {
		panic(errors.New("f must be a function"))
	} else if fTy.IsVariadic() {
		panic(errors.New("f is variadic"))
	}

	var argValues []reflect.Value
	for _, a := range args {
		argValues = append(argValues, reflect.ValueOf(a))
	}

	*a.waitingOnPreparedCall = true

	return PreparedFunctionCall{a: a, f: fv, args: argValues}
}

// PreparedFunctionCall is a function call that has been set up by (Assert).Call() but not executed
type PreparedFunctionCall struct {
	a    Assert
	f    reflect.Value
	args []reflect.Value
}

// Equals calls the prepared function, checking that all the return values are equal to what's
// expected, and that no unexpected warnings were generated.
func (f PreparedFunctionCall) Equals(expected ...any) {
	*f.a.waitingOnPreparedCall = false

	fTy := f.f.Type()

	numOut := fTy.NumOut()
	if len(expected) != numOut {
		panic(fmt.Errorf(
			"mismatched number of out parameters from function: func has %d but expected len is %d",
			numOut,
			len(expected),
		))
	}

	type unknownInterface any

	var actualReturnTypes []reflect.Type
	var expectedReturnTypes []reflect.Type
	for i := 0; i < numOut; i += 1 {
		actual := fTy.Out(i)
		actualReturnTypes = append(actualReturnTypes, actual)

		// Can't call reflect.Value.Type on nil, so if we're given a nil value, we have to be a
		// little more permissive.
		var expectedTy reflect.Type
		if expected[i] != nil {
			expectedTy = reflect.TypeOf(expected[i])
		} else if actual.Kind() == reflect.Interface {
			// well, the actual value can be a nil interface too, so it's probably fine
			expectedTy = actual
		} else {
			// but... if the actual value isn't an interface, there's a problem
			expectedTy = reflect.TypeOf((*unknownInterface)(nil)).Elem()
		}
		expectedReturnTypes = append(expectedReturnTypes, expectedTy)
	}

	if !reflect.DeepEqual(expectedReturnTypes, actualReturnTypes) {
		panic(fmt.Errorf(
			"provided return types not equal to the function's: function has %v, but expected has %v",
			actualReturnTypes,
			expectedReturnTypes,
		))
	}

	returnValues := f.f.Call(f.args)
	for i := range returnValues {
		assert.Equal(f.a.t, expected[i], returnValues[i].Interface())
	}
	assert.Equal(f.a.t, f.a.tinfo.expectedWarnings, *f.a.storedWarnings)
	if f.a.t.Failed() {
		f.a.t.FailNow()
	}
	*f.a.storedWarnings = []string{}
}
