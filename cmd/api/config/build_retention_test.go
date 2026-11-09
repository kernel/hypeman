package config

import (
	"strings"
	"testing"
	"time"
)

func TestBuildRetention(t *testing.T) {
	for _, value := range []string{"", "invalid", "0", "-1h", "30m"} {
		t.Run(value, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Build.Retention = value
			err := cfg.Validate()
			if value == "30m" {
				if err != nil || cfg.Build.RetentionDuration != 30*time.Minute {
					t.Fatalf("retention = %s, error = %v", cfg.Build.RetentionDuration, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "build.retention") {
				t.Fatalf("expected build.retention validation error, got %v", err)
			}
		})
	}
}
