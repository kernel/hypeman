package vz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSaveRestoreSupported(t *testing.T) {
	tests := []struct {
		name           string
		goos           string
		goarch         string
		productVersion string
		want           bool
	}{
		{name: "linux arm64 is never vz", goos: "linux", goarch: "arm64", productVersion: "14.5", want: false},
		{name: "darwin amd64 unsupported", goos: "darwin", goarch: "amd64", productVersion: "14.5", want: false},
		{name: "macOS 13 overstates nothing", goos: "darwin", goarch: "arm64", productVersion: "13.6", want: false},
		{name: "macOS 14 minimum", goos: "darwin", goarch: "arm64", productVersion: "14.0", want: true},
		{name: "macOS 14 patch", goos: "darwin", goarch: "arm64", productVersion: "14.5.1", want: true},
		{name: "macOS 15", goos: "darwin", goarch: "arm64", productVersion: "15.2", want: true},
		{name: "macOS 26", goos: "darwin", goarch: "arm64", productVersion: "26.0", want: true},
		{name: "empty version is unsupported", goos: "darwin", goarch: "arm64", productVersion: "", want: false},
		{name: "unparsable version is unsupported", goos: "darwin", goarch: "arm64", productVersion: "unknown", want: false},
		{name: "major-only version", goos: "darwin", goarch: "arm64", productVersion: "14", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, SaveRestoreSupported(tt.goos, tt.goarch, tt.productVersion))
		})
	}
}
