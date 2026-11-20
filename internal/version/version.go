// Package version holds build metadata injected at link time via -ldflags.
package version

import (
	"fmt"
	"runtime"
)

// Set by the linker: -X github.com/monirujjaman-dev/docklet/internal/version.Version=...
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Info is the JSON-friendly view of the build metadata.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Get returns the current build metadata.
func Get() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

// String returns a single-line human-readable version string.
func (i Info) String() string {
	return fmt.Sprintf("docklet %s (commit %s, built %s, %s %s/%s)",
		i.Version, i.Commit, i.Date, i.Go, i.OS, i.Arch)
}
