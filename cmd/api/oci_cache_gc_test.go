package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"golang.org/x/sync/errgroup"
)

type stubOCICacheGCRunner struct {
	runCount atomic.Int32
}

func (s *stubOCICacheGCRunner) Run(ctx context.Context) error {
	s.runCount.Add(1)
	<-ctx.Done()
	return nil
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load temp config: %v", err)
	}
	return cfg
}

func TestConfigureOCICacheGCSkipsDisabledConfig(t *testing.T) {
	cfg := loadTestConfig(t)

	runner, err := configureOCICacheGC(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatalf("configure disabled oci cache gc: %v", err)
	}
	if runner != nil {
		t.Fatalf("expected disabled oci cache gc to return nil runner")
	}
}

func TestConfigureOCICacheGCBuildsCollectorWhenEnabled(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Images.OCICacheGC.Enabled = true
	cfg.Images.OCICacheGC.Interval = "2m"
	cfg.Images.OCICacheGC.MinBlobAge = "30s"

	runner, err := configureOCICacheGC(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatalf("configure enabled oci cache gc: %v", err)
	}
	if runner == nil {
		t.Fatalf("expected enabled oci cache gc to return runner")
	}
}

func TestConfigureOCICacheGCRejectsInvalidInterval(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Images.OCICacheGC.Enabled = true
	cfg.Images.OCICacheGC.Interval = "0s"

	if _, err := configureOCICacheGC(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil); err == nil {
		t.Fatalf("expected invalid oci cache gc interval to fail")
	}
}

func TestStartOCICacheGCSkipsNilRunner(t *testing.T) {
	grp, ctx := errgroup.WithContext(context.Background())

	started := startOCICacheGC(grp, ctx, nil)
	if started {
		t.Fatalf("expected nil oci cache gc runner not to start")
	}
}

func TestStartOCICacheGCStartsRunner(t *testing.T) {
	grp, ctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	runner := &stubOCICacheGCRunner{}
	started := startOCICacheGC(grp, ctx, runner)
	if !started {
		t.Fatalf("expected oci cache gc runner to start")
	}

	deadline := time.Now().Add(time.Second)
	for runner.runCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runner.runCount.Load() != 1 {
		t.Fatalf("expected runner to be started once, got %d", runner.runCount.Load())
	}

	cancel()
	if err := grp.Wait(); err != nil {
		t.Fatalf("wait for oci cache gc runner: %v", err)
	}
}
