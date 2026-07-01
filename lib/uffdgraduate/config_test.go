package uffdgraduate

import (
	"testing"
	"time"
)

func TestConfigNormalizeDefaults(t *testing.T) {
	got := Config{Enabled: true}.Normalize()
	if got.MinSessionAge != defaultMinSessionAge {
		t.Fatalf("MinSessionAge = %s, want %s", got.MinSessionAge, defaultMinSessionAge)
	}
	if got.MaxConcurrent != defaultMaxConcurrent {
		t.Fatalf("MaxConcurrent = %d, want %d", got.MaxConcurrent, defaultMaxConcurrent)
	}
	if got.ScanInterval != defaultScanInterval {
		t.Fatalf("ScanInterval = %s, want %s", got.ScanInterval, defaultScanInterval)
	}
	if got.CompletionTimeout != defaultCompletionTimeout {
		t.Fatalf("CompletionTimeout = %s, want %s", got.CompletionTimeout, defaultCompletionTimeout)
	}
}

func TestConfigNormalizeKeepsExplicit(t *testing.T) {
	in := Config{
		Enabled:           true,
		MinSessionAge:     2 * time.Minute,
		MaxConcurrent:     4,
		ScanInterval:      30 * time.Second,
		CompletionTimeout: time.Minute,
	}
	got := in.Normalize()
	if got != in {
		t.Fatalf("Normalize changed explicit config: got %+v want %+v", got, in)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{Enabled: false, MinSessionAge: -1}).Validate(); err != nil {
		t.Fatalf("disabled config should always validate, got %v", err)
	}
	if err := (Config{Enabled: true, MaxConcurrent: -1}).Validate(); err == nil {
		t.Fatal("expected error for negative max_concurrent")
	}
	if err := (Config{Enabled: true, MinSessionAge: time.Minute}).Validate(); err != nil {
		t.Fatalf("valid enabled config should pass, got %v", err)
	}
}
