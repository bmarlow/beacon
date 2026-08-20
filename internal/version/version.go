/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package version exposes the operator build version. Version is injected at
// build time via -ldflags "-X github.com/beacon-operator/beacon/internal/version.Version=<v>"
// (see the Dockerfile / CI). When unset it falls back to the Go build info main
// module version, then to "dev".
package version

import "runtime/debug"

// Version is the operator build version. Overridden at link time.
var Version = ""

// Get returns the effective version string.
func Get() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		// Fall back to the VCS revision embedded by `go build`.
		var rev, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if rev != "" {
			short := rev
			if len(short) > 12 {
				short = short[:12]
			}
			if modified == "true" {
				return short + "-dirty"
			}
			return short
		}
	}
	return "dev"
}
