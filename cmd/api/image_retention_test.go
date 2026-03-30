package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

type stubImageRetentionRunner struct {
	runCount atomic.Int32
}

func (s *stubImageRetentionRunner) Run(ctx context.Context) error {
	s.runCount.Add(1)
	<-ctx.Done()
	return nil
}

func TestStartImageRetentionControllerSkipsNilController(t *testing.T) {
	grp, ctx := errgroup.WithContext(context.Background())

	started := startImageRetentionController(grp, ctx, nil)
	if started {
		t.Fatalf("expected nil controller not to start")
	}
}

func TestStartImageRetentionControllerStartsRunner(t *testing.T) {
	grp, ctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	runner := &stubImageRetentionRunner{}
	started := startImageRetentionController(grp, ctx, runner)
	if !started {
		t.Fatalf("expected controller to start")
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
		t.Fatalf("wait for retention runner: %v", err)
	}
}
