package stack

// Originally taken from https://github.com/sharnoff/chord

// TODO - want to have some kind of "N skipped" when (a) there's lots of frames and (b) many of
// those frames are duplicates

import (
	"runtime"
	"strconv"
	"sync"
)

// StackTrace represents a collected stack trace, possibly with a parent (i.e caller)
//
// StackTraces are designed to make it easy to track callers across goroutines. They are typically
// produced by [GetStackTrace]; refer to that function for more information.
type StackTrace struct {
	// Frames provides the frames of this stack trace. Each frame's caller is at the index following
	// it; the first frame is the direct caller.
	Frames []StackFrame
	// Parent, if not nil, provides the "parent" stack trace - typically the stack trace at the
	// point this goroutine was spawned.
	Parent *StackTrace
}

// Individual stack frame, contained in a [StackTrace], produced by [GetStackTrace].
type StackFrame struct {
	// Function provides the name of the function being called, or the empty string if unknown.
	Function string
	// File gives the name of the file, or an empty string if the file is unknown.
	File string
	// Line gives the line number (starting from 1), or zero if the line number is unknown.
	Line int
}

// GetStackTrace produces a StackTrace, optionally with a parent's stack trace to append.
//
// skip sets the number of initial calling stack frames to exclude. Setting skip to zero will
// produce a StackTrace where the first [StackFrame] represents the location where GetStackTrace was
// called.
func GetStackTrace(parent *StackTrace, skip uint) StackTrace {
	frames := getFrames(skip + 1) // skip the additional frame introduced by GetStackTrace
	return StackTrace{Frames: frames, Parent: parent}
}

// String produces a string representation of the stack trace, roughly similar to the default panic
// handler's.
//
// For some examples of formatting, refer to the StackTrace tests.
func (st StackTrace) String() string {
	var buf []byte

	for {
		if len(st.Frames) == 0 {
			buf = append(buf, "<empty stack>\n"...)
		} else {
			for _, f := range st.Frames {
				var function, functionTail, file, fileLineSep, line string

				if f.Function == "" {
					function = "<unknown function>"
				} else {
					function = f.Function
					functionTail = "(...)"
				}

				if f.File == "" {
					file = "<unknown file>"
				} else {
					file = f.File
					if f.Line != 0 {
						fileLineSep = ":"
						line = strconv.Itoa(f.Line)
					}
				}

				buf = append(buf, function...)
				buf = append(buf, functionTail...)
				buf = append(buf, "\n\t"...)
				buf = append(buf, file...)
				buf = append(buf, fileLineSep...)
				buf = append(buf, line...)
				buf = append(buf, byte('\n'))
			}
		}

		if st.Parent == nil {
			break
		}

		st = *st.Parent
		buf = append(buf, "called by "...)
		continue
	}

	return string(buf)
}

var pcBufPool = sync.Pool{
	New: func() any {
		buf := make([]uintptr, 128)
		return &buf
	},
}

func putPCBuffer(buf *[]uintptr) {
	if len(*buf) < 1024 {
		pcBufPool.Put(buf)
	}
}

func getFrames(skip uint) []StackFrame {
	skip += 2 // skip the frame introduced by this function and runtime.Callers

	pcBuf := pcBufPool.Get().(*[]uintptr)
	defer putPCBuffer(pcBuf)
	if len(*pcBuf) == 0 {
		panic("internal error: len(*pcBuf) == 0")
	}

	// read program counters into the buffer, repeating until buffer is big enough.
	//
	// This is O(n log n), where n is the true number of program counters.
	var pc []uintptr
	for {
		n := runtime.Callers(0, *pcBuf)
		if n == 0 {
			panic("runtime.Callers(0, ...) returned zero")
		}

		if n < len(*pcBuf) {
			pc = (*pcBuf)[:n]
			break
		} else {
			*pcBuf = make([]uintptr, 2*len(*pcBuf))
		}
	}

	framesIter := runtime.CallersFrames(pc)
	var frames []StackFrame
	more := true
	for more {
		var frame runtime.Frame
		frame, more = framesIter.Next()

		if skip > 0 {
			skip -= 1
			continue
		}

		frames = append(frames, StackFrame{
			Function: frame.Function,
			File:     frame.File,
			Line:     frame.Line,
		})
	}

	return frames
}
