package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if cfg.Otel.MetricExportInterval != "60s" {
		t.Fatalf("expected default otel.metric_export_interval to be 60s, got %q", cfg.Otel.MetricExportInterval)
	}
}

func TestLoadEnvOverridesMetricsAndOtelInterval(t *testing.T) {
	t.Setenv("METRICS__LISTEN_ADDRESS", "0.0.0.0")
	t.Setenv("METRICS__PORT", "9999")
	t.Setenv("METRICS__VM_LABEL_BUDGET", "350")
	t.Setenv("OTEL__METRIC_EXPORT_INTERVAL", "15s")

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
	if cfg.Otel.MetricExportInterval != "15s" {
		t.Fatalf("expected otel.metric_export_interval override, got %q", cfg.Otel.MetricExportInterval)
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

func TestValidateRejectsInvalidVMLabelBudget(t *testing.T) {
	cfg := defaultConfig()
	cfg.Metrics.VMLabelBudget = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid vm label budget")
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
