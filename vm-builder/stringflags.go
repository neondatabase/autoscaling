package main

import (
	"flag"
	"strings"
)

type stringsFlagsVar []string

func (s *stringsFlagsVar) String() string {
	return strings.Join(*s, ", ")
}

func (s *stringsFlagsVar) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func MultiStringFlag(name string, usage string) *stringsFlagsVar {
	sv := new(stringsFlagsVar)
	flag.Var(sv, name, usage)
	return sv
}
