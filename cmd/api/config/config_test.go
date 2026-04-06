package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/lib/guestmemory"
)

func TestDefaultConfigIncludesMetricsSettings(t *testing.T) {
	cfg := defaultConfig()

	if cfg.Metrics.ListenAddress != "127.0.0.1" {
		t.Fatalf("expected default metrics.listen_address to be 127.0.0.1, got %q", cfg.Metrics.ListenAddress)
	}
	if cfg.Metrics.Port != 9464 {
		t.Fatalf("expected default metrics.port to be 9464, got %d", cfg.Metrics.Port)
	}
	if cfg.Metrics.VMLabelBudget != 200 {
		t.Fatalf("expected default metrics.vm_label_budget to be 200, got %d", cfg.Metrics.VMLabelBudget)
	}
	if cfg.Metrics.ResourceRefreshInterval != "120s" {
		t.Fatalf("expected default metrics.resource_refresh_interval to be 120s, got %q", cfg.Metrics.ResourceRefreshInterval)
	}
	if cfg.Otel.MetricExportInterval != "60s" {
		t.Fatalf("expected default otel.metric_export_interval to be 60s, got %q", cfg.Otel.MetricExportInterval)
	}
	if cfg.Otel.SuccessfulGetSampleRatio != 0.1 {
		t.Fatalf("expected default otel.successful_get_sample_ratio to be 0.1, got %v", cfg.Otel.SuccessfulGetSampleRatio)
	}
	if cfg.Images.AutoDelete.Enabled {
		t.Fatalf("expected default images.auto_delete.enabled to be false")
	}
	if cfg.Images.AutoDelete.UnusedFor != "720h" {
		t.Fatalf("expected default images.auto_delete.unused_for to be 720h, got %q", cfg.Images.AutoDelete.UnusedFor)
	}
	if len(cfg.Images.AutoDelete.Allowed) != 0 {
		t.Fatalf("expected default images.auto_delete.allowed to be empty, got %v", cfg.Images.AutoDelete.Allowed)
	}
	if cfg.Instances.LifecycleEventBufferSize != 256 {
		t.Fatalf("expected default instances.lifecycle_event_buffer_size to be 256, got %d", cfg.Instances.LifecycleEventBufferSize)
	}
}

func TestLoadEnvOverridesMetricsAndOtelInterval(t *testing.T) {
	t.Setenv("METRICS__LISTEN_ADDRESS", "0.0.0.0")
	t.Setenv("METRICS__PORT", "9999")
	t.Setenv("METRICS__VM_LABEL_BUDGET", "350")
	t.Setenv("METRICS__RESOURCE_REFRESH_INTERVAL", "30s")
	t.Setenv("OTEL__METRIC_EXPORT_INTERVAL", "15s")
	t.Setenv("OTEL__SUCCESSFUL_GET_SAMPLE_RATIO", "0.25")
	t.Setenv("INSTANCES__LIFECYCLE_EVENT_BUFFER_SIZE", "512")

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("{}\n"), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Metrics.ListenAddress != "0.0.0.0" {
		t.Fatalf("expected metrics.listen_address override, got %q", cfg.Metrics.ListenAddress)
	}
	if cfg.Metrics.Port != 9999 {
		t.Fatalf("expected metrics.port override, got %d", cfg.Metrics.Port)
	}
	if cfg.Metrics.VMLabelBudget != 350 {
		t.Fatalf("expected metrics.vm_label_budget override, got %d", cfg.Metrics.VMLabelBudget)
	}
	if cfg.Metrics.ResourceRefreshInterval != "30s" {
		t.Fatalf("expected metrics.resource_refresh_interval override, got %q", cfg.Metrics.ResourceRefreshInterval)
	}
	if cfg.Otel.MetricExportInterval != "15s" {
		t.Fatalf("expected otel.metric_export_interval override, got %q", cfg.Otel.MetricExportInterval)
	}
	if cfg.Otel.SuccessfulGetSampleRatio != 0.25 {
		t.Fatalf("expected otel.successful_get_sample_ratio override, got %v", cfg.Otel.SuccessfulGetSampleRatio)
	}
	if cfg.Instances.LifecycleEventBufferSize != 512 {
		t.Fatalf("expected instances.lifecycle_event_buffer_size override, got %d", cfg.Instances.LifecycleEventBufferSize)
	}
}

func TestValidateRejectsInvalidMetricsPort(t *testing.T) {
	cfg := defaultConfig()
	cfg.Metrics.Port = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid metrics port")
	}
}

func TestValidateRejectsInvalidMetricExportInterval(t *testing.T) {
	cfg := defaultConfig()
	cfg.Otel.MetricExportInterval = "not-a-duration"

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid metric export interval")
	}
}

func TestValidateRejectsInvalidSuccessfulGetSampleRatio(t *testing.T) {
	cfg := defaultConfig()
	cfg.Otel.SuccessfulGetSampleRatio = 1.1

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid successful get sample ratio")
	}
}

func TestValidateRejectsInvalidVMLabelBudget(t *testing.T) {
	cfg := defaultConfig()
	cfg.Metrics.VMLabelBudget = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid vm label budget")
	}
}

func TestValidateRejectsInvalidResourceRefreshInterval(t *testing.T) {
	cfg := defaultConfig()
	cfg.Metrics.ResourceRefreshInterval = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for empty resource refresh interval")
	}

	cfg = defaultConfig()
	cfg.Metrics.ResourceRefreshInterval = "not-a-duration"

	err = cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid resource refresh interval")
	}

	cfg = defaultConfig()
	cfg.Metrics.ResourceRefreshInterval = "0s"

	err = cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for non-positive resource refresh interval")
	}
}

func TestLoadUsesConfiguredLifecycleEventBufferSize(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("instances:\n  lifecycle_event_buffer_size: 384\n"), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Instances.LifecycleEventBufferSize != 384 {
		t.Fatalf("expected instances.lifecycle_event_buffer_size from config file, got %d", cfg.Instances.LifecycleEventBufferSize)
	}
}

func TestValidateRejectsInvalidLifecycleEventBufferSize(t *testing.T) {
	cfg := defaultConfig()
	cfg.Instances.LifecycleEventBufferSize = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid lifecycle event buffer size")
	}
}

func TestLoadUsesDefaultImageAutoDeleteRetentionWindow(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("images:\n  auto_delete:\n    enabled: true\n"), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if !cfg.Images.AutoDelete.Enabled {
		t.Fatalf("expected images.auto_delete.enabled override to be true")
	}
	if cfg.Images.AutoDelete.UnusedFor != "720h" {
		t.Fatalf("expected default images.auto_delete.unused_for to remain 720h, got %q", cfg.Images.AutoDelete.UnusedFor)
	}
	if len(cfg.Images.AutoDelete.Allowed) != 0 {
		t.Fatalf("expected default images.auto_delete.allowed to remain empty, got %v", cfg.Images.AutoDelete.Allowed)
	}
}

func TestValidateRejectsInvalidImageAutoDeleteUnusedFor(t *testing.T) {
	cfg := defaultConfig()
	cfg.Images.AutoDelete.UnusedFor = "not-a-duration"

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid images.auto_delete.unused_for")
	}
}

func TestValidateTrimsImageAutoDeleteAllowedPatterns(t *testing.T) {
	cfg := defaultConfig()
	cfg.Images.AutoDelete.Allowed = []string{"  docker.io/library/*  ", "   ", "ghcr.io/kernel/*"}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected image auto delete allow list to validate, got %v", err)
	}
	if cfg.Images.AutoDelete.Allowed[0] != "docker.io/library/*" {
		t.Fatalf("expected first allow pattern to be trimmed, got %q", cfg.Images.AutoDelete.Allowed[0])
	}
	if cfg.Images.AutoDelete.Allowed[1] != "" {
		t.Fatalf("expected whitespace-only allow pattern to trim to empty string, got %q", cfg.Images.AutoDelete.Allowed[1])
	}
	if cfg.Images.AutoDelete.Allowed[2] != "ghcr.io/kernel/*" {
		t.Fatalf("expected third allow pattern to be trimmed, got %q", cfg.Images.AutoDelete.Allowed[2])
	}
}

func TestValidateRejectsEmptyActiveBallooningDurations(t *testing.T) {
	cfg := defaultConfig()
	cfg.Hypervisor.Memory.ActiveBallooning.PollInterval = "   "

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "poll_interval must not be empty") {
		t.Fatalf("expected poll_interval empty validation error, got %v", err)
	}

	cfg = defaultConfig()
	cfg.Hypervisor.Memory.ActiveBallooning.PerVmCooldown = ""

	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "per_vm_cooldown must not be empty") {
		t.Fatalf("expected per_vm_cooldown empty validation error, got %v", err)
	}
}

func TestDefaultConfigActiveBallooningMatchesGoDefaults(t *testing.T) {
	cfg := defaultConfig()
	want := guestmemory.DefaultActiveBallooningConfig()

	parse := func(value string) int64 {
		t.Helper()

		var size datasize.ByteSize
		if err := size.UnmarshalText([]byte(value)); err != nil {
			t.Fatalf("parse default byte size %q: %v", value, err)
		}
		return int64(size)
	}

	if got := parse(cfg.Hypervisor.Memory.ActiveBallooning.ProtectedFloorMinBytes); got != want.ProtectedFloorMinBytes {
		t.Fatalf("protected floor default mismatch: got %d want %d", got, want.ProtectedFloorMinBytes)
	}
	if got := parse(cfg.Hypervisor.Memory.ActiveBallooning.MinAdjustmentBytes); got != want.MinAdjustmentBytes {
		t.Fatalf("min adjustment default mismatch: got %d want %d", got, want.MinAdjustmentBytes)
	}
	if got := parse(cfg.Hypervisor.Memory.ActiveBallooning.PerVmMaxStepBytes); got != want.PerVMMaxStepBytes {
		t.Fatalf("per-vm max step default mismatch: got %d want %d", got, want.PerVMMaxStepBytes)
	}
}

func TestValidateAllowsLZ4CompressionDefaultWithImplicitLevel(t *testing.T) {
	cfg := defaultConfig()
	cfg.Snapshot.CompressionDefault.Enabled = true
	cfg.Snapshot.CompressionDefault.Algorithm = "LZ4"
	cfg.Snapshot.CompressionDefault.Level = nil

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected lz4 compression default to validate, got %v", err)
	}
	if cfg.Snapshot.CompressionDefault.Algorithm != "lz4" {
		t.Fatalf("expected algorithm to normalize to lowercase, got %q", cfg.Snapshot.CompressionDefault.Algorithm)
	}
}

func TestValidateAllowsExplicitLZ4CompressionLevelRange(t *testing.T) {
	cfg := defaultConfig()
	cfg.Snapshot.CompressionDefault.Enabled = true
	cfg.Snapshot.CompressionDefault.Algorithm = "lz4"
	cfg.Snapshot.CompressionDefault.Level = intPtr(9)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected lz4 level to validate, got %v", err)
	}
}

func TestValidateRejectsInvalidLZ4CompressionLevel(t *testing.T) {
	cfg := defaultConfig()
	cfg.Snapshot.CompressionDefault.Enabled = true
	cfg.Snapshot.CompressionDefault.Algorithm = "lz4"
	cfg.Snapshot.CompressionDefault.Level = intPtr(10)

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid lz4 level")
	}
}

func TestValidateAllowsDisabledSnapshotCompressionDefaultWithoutValidAlgorithm(t *testing.T) {
	cfg := defaultConfig()
	cfg.Snapshot.CompressionDefault.Enabled = false
	cfg.Snapshot.CompressionDefault.Algorithm = "definitely-not-real"
	cfg.Snapshot.CompressionDefault.Level = intPtr(999)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected disabled snapshot compression default to ignore algorithm/level, got %v", err)
	}
}
