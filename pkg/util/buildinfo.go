package util

// This file primarily exposes the GetBuildInfo function

import (
	"runtime/debug"
)

// BuildGitInfo stores some pretty-formatted information about the repository and working tree at
// build time. It's set by the GIT_INFO argument in the Dockerfiles and generated with the git_info
// function in 'scripts-common.sh'.
//
// While public, this value is not expected to be used externally. You should use GetBuildInfo
// instead.
var BuildGitInfo string

// BuildInfo stores a little bit of information about the build of the current binary
//
// All strings are guaranteed to be non-empty.
type BuildInfo struct {
	GitInfo   string
	GoVersion string
}

// GetBuildInfo makes a best-effort attempt to return some information about how the currently
// running binary was built
func GetBuildInfo() BuildInfo {
	goVersion := "<unknown>"
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		if buildInfo.GoVersion != "" {
			goVersion = buildInfo.GoVersion
		}
	}

	gitInfo := BuildGitInfo
	if BuildGitInfo == "" {
		gitInfo = "<unknown>"
	}

	return BuildInfo{
		GitInfo:   gitInfo,
		GoVersion: goVersion,
	}
}
