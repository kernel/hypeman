package guest

import (
	"context"
	"fmt"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SyncClockOptions configures a guest clock sync
type SyncClockOptions struct {
	WaitForAgent time.Duration // Max time to wait for agent to be ready (0 = no wait, fail immediately)
}

// SyncClockInInstance sets the guest realtime clock to the host's current
// time. Guest clocks resume from the snapshot's saved time after a standby
// restore, so this is called post-restore to remove the accumulated skew.
func SyncClockInInstance(ctx context.Context, dialer hypervisor.VsockDialer, opts SyncClockOptions) error {
	if opts.WaitForAgent == 0 {
		err := syncClockOnce(ctx, dialer)
		if err != nil && isRetryableConnectionError(err) {
			CloseConn(dialer.Key())
		}
		return err
	}

	ctx, span := guestTracer().Start(ctx, "guest.sync_clock", trace.WithAttributes(
		attribute.Bool("wait_for_agent", true),
		attribute.Int64("wait_for_agent_ms", opts.WaitForAgent.Milliseconds()),
	))
	defer span.End()

	deadline := time.Now().Add(opts.WaitForAgent)
	start := time.Now()
	attempts := 0
	retryableAttempts := 0
	firstRetryableErrorType := ""
	lastRetryableErrorType := ""
	lastRetryInterval := time.Duration(0)

	for {
		attempts++
		err := syncClockOnce(ctx, dialer)
		if err == nil {
			recordGuestExecWait(span, start, attempts, retryableAttempts, firstRetryableErrorType, lastRetryableErrorType, lastRetryInterval)
			span.SetStatus(otelcodes.Ok, "")
			return nil
		}
		if !isRetryableConnectionError(err) {
			recordGuestExecWait(span, start, attempts, retryableAttempts, firstRetryableErrorType, lastRetryableErrorType, lastRetryInterval)
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return err
		}

		retryableAttempts++
		errType := retryableConnectionErrorType(err)
		if firstRetryableErrorType == "" {
			firstRetryableErrorType = errType
		}
		lastRetryableErrorType = errType
		CloseConn(dialer.Key())

		if time.Now().After(deadline) {
			recordGuestExecWait(span, start, attempts, retryableAttempts, firstRetryableErrorType, lastRetryableErrorType, lastRetryInterval)
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return err
		}

		retryInterval := guestExecRetryInterval(time.Since(start))
		lastRetryInterval = retryInterval
		select {
		case <-ctx.Done():
			recordGuestExecWait(span, start, attempts, retryableAttempts, firstRetryableErrorType, lastRetryableErrorType, lastRetryInterval)
			span.RecordError(ctx.Err())
			span.SetStatus(otelcodes.Error, ctx.Err().Error())
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

func syncClockOnce(ctx context.Context, dialer hypervisor.VsockDialer) error {
	grpcConn, err := GetOrCreateConn(ctx, dialer)
	if err != nil {
		return fmt.Errorf("get grpc connection: %w", err)
	}
	client := NewGuestServiceClient(grpcConn)

	_, span := guestTracer().Start(ctx, "guest.sync_clock.rpc")
	// Capture the host time per attempt so retries after a long agent wait
	// don't bake the wait into the guest clock.
	_, err = client.SyncClock(ctx, &SyncClockRequest{
		UnixNanos: time.Now().UnixNano(),
	})
	finishGuestExecStepSpan(span, err)
	if err != nil {
		return fmt.Errorf("sync clock rpc: %w", err)
	}
	return nil
}
