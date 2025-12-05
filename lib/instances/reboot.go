package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/vmm"
	"go.opentelemetry.io/otel/trace"
)

// rebootInstance reboots a running instance
// The VM stays in Running state throughout
func (m *manager) rebootInstance(
	ctx context.Context,
	id string,
) (*Instance, error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "rebooting instance", "id", id)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "RebootInstance")
		defer span.End()
	}

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	log.DebugContext(ctx, "loaded instance", "id", id, "state", inst.State)

	// 2. Validate state (must be Running to reboot)
	if inst.State != StateRunning {
		log.ErrorContext(ctx, "invalid state for reboot", "id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot reboot from state %s, must be Running", ErrInvalidState, inst.State)
	}

	// 3. Create VMM client
	client, err := vmm.NewVMM(inst.SocketPath)
	if err != nil {
		log.ErrorContext(ctx, "failed to create VMM client", "id", id, "error", err)
		return nil, fmt.Errorf("create vmm client: %w", err)
	}

	// 4. Send reboot command to VM
	log.DebugContext(ctx, "sending reboot to VM", "id", id)
	rebootResp, err := client.RebootVMWithResponse(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to send reboot to VM", "id", id, "error", err)
		return nil, fmt.Errorf("reboot vm: %w", err)
	}
	if rebootResp.StatusCode() != 204 {
		log.ErrorContext(ctx, "reboot VM returned error", "id", id, "status", rebootResp.StatusCode())
		return nil, fmt.Errorf("reboot vm failed with status %d", rebootResp.StatusCode())
	}

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.rebootDuration, start, "success")
		// Reboot is Running → Running, so record that transition
		m.recordStateTransition(ctx, string(StateRunning), string(StateRunning))
	}

	// Return instance (should still be Running)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance rebooted successfully", "id", id, "state", finalInst.State)
	return &finalInst, nil
}
