// Package version reports the build of karga and karga-server.
package version

import (
	"runtime"
	"runtime/debug"
)

// Version is set at build time: -ldflags "-X github.com/hasankaradmir/karga-server/internal/version.Version=v0.1.0".
var Version = "dev"

// Info describes a build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"goVersion"`
}

// Current returns this binary's build.
func Current() Info {
	v := Info{Version: Version, Commit: "unknown", GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				v.Commit = s.Value
			}
		}
	}
	return v
}
