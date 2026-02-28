package cmd

import (
	"regexp"
	"runtime/debug"
)

// version can be overridden at build time via ldflags:
//
//	-X github.com/kernel/hypeman-cli/pkg/cmd.version=1.2.3
var version string

// semverTag matches clean semver tags like v1.2.3 or v0.9.5 (no prerelease/pseudo-version suffix).
var semverTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// Version is the CLI version string, resolved at init time.
var Version = resolveVersion()

func resolveVersion() string {
	// 1. ldflags override (GoReleaser sets this)
	if version != "" {
		return version
	}

	// 2. Build info from Go toolchain
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}

	// 3. VCS revision from git (embedded automatically by `go build`)
	var revision string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	// Only use module version if it's a clean semver tag (e.g. v1.2.3),
	// not a pseudo-version like v0.9.5-0.20260211212111-7ef5ed6df05d.
	if v := info.Main.Version; semverTag.MatchString(v) {
		return v
	}

	if revision != "" {
		short := revision
		if len(short) > 7 {
			short = short[:7]
		}
		if dirty {
			return short + "-dirty"
		}
		return short
	}

	return "dev"
}
