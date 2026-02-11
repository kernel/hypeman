package cmd

import "runtime/debug"

// version can be overridden at build time via ldflags:
//
//	-X github.com/kernel/hypeman-cli/pkg/cmd.version=1.2.3
var version string

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

	// Module version is set by `go install module@vX.Y.Z`
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
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
