// Package buildinfo identifies the complete release, rather than individual workers.
package buildinfo

import "runtime"

// ReleaseID is set to the same immutable identifier in both executables by the build.
var ReleaseID = "dev"

type Info struct {
	ReleaseID  string `json:"release_id"`
	GoVersion  string `json:"go_version"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	APIVersion int    `json:"api_version"`
}

func Current() Info {
	return Info{ReleaseID, runtime.Version(), runtime.GOOS, runtime.GOARCH, 1}
}
