package config

import (
	"strings"
	"testing"
)

func TestDesktopOriginConfig(t *testing.T) {
	for _, origin := range []string{"", "https://api.example", "http://127.0.0.1:4974/"} {
		cfg := defaultConfig()
		cfg.MacOSDesktopOrigin = origin
		if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "macos_desktop_origin") {
			t.Fatalf("valid origin rejected: %v", err)
		}
	}
	for _, origin := range []string{"ws://api.example", "https://user:pass@api.example", "https://api.example/path", "https://api.example?token=x", "https://api.example#fragment"} {
		cfg := defaultConfig()
		cfg.MacOSDesktopOrigin = origin
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "macos_desktop_origin") {
			t.Fatalf("invalid origin accepted: %s (%v)", origin, err)
		}
	}
}
