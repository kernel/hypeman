package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/trace"
)

// startInstance starts a stopped instance
// Transition: Stopped → Running
func (m *manager) startInstance(
	ctx context.Context,
	id string,
) (*Instance, error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "starting instance", "id", id)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "StartInstance")
		defer span.End()
	}

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	log.DebugContext(ctx, "loaded instance", "id", id, "state", inst.State)

	// 2. Validate state (must be Stopped to start)
	if inst.State != StateStopped {
		log.ErrorContext(ctx, "invalid state for start", "id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot start from state %s, must be Stopped", ErrInvalidState, inst.State)
	}

	// 3. Get image info (needed for buildVMConfig)
	log.DebugContext(ctx, "getting image info", "id", id, "image", stored.Image)
	imageInfo, err := m.imageManager.GetImage(ctx, stored.Image)
	if err != nil {
		log.ErrorContext(ctx, "failed to get image", "id", id, "image", stored.Image, "error", err)
		return nil, fmt.Errorf("get image: %w", err)
	}

	// 4. Recreate network allocation if network enabled
	var netConfig *network.NetworkConfig
	if stored.NetworkEnabled {
		log.DebugContext(ctx, "recreating network for start", "id", id, "network", "default")
		if err := m.networkManager.RecreateAllocation(ctx, id); err != nil {
			log.ErrorContext(ctx, "failed to recreate network", "id", id, "error", err)
			return nil, fmt.Errorf("recreate network: %w", err)
		}
		// Get the network config for VM configuration
		netAlloc, err := m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.ErrorContext(ctx, "failed to get network allocation", "id", id, "error", err)
			// Cleanup network on failure
			if netAlloc != nil {
				m.networkManager.ReleaseAllocation(ctx, netAlloc)
			}
			return nil, fmt.Errorf("get network allocation: %w", err)
		}
		netConfig = &network.NetworkConfig{
			TAPDevice: netAlloc.TAPDevice,
			IP:        netAlloc.IP,
			MAC:       netAlloc.MAC,
			Netmask:   "255.255.255.0", // Default netmask
		}
	}

	// 5. Start VMM and boot VM (reuses logic from create)
	log.InfoContext(ctx, "starting VMM and booting VM", "id", id)
	if err := m.startAndBootVM(ctx, stored, imageInfo, netConfig); err != nil {
		log.ErrorContext(ctx, "failed to start and boot VM", "id", id, "error", err)
		// Cleanup network on failure
		if stored.NetworkEnabled {
			if netAlloc, err := m.networkManager.GetAllocation(ctx, id); err == nil {
				m.networkManager.ReleaseAllocation(ctx, netAlloc)
			}
		}
		return nil, err
	}

	// 6. Update metadata (set PID, StartedAt)
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed - log but don't fail
		log.WarnContext(ctx, "failed to update metadata after VM start", "id", id, "error", err)
	}

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.startDuration, start, "success")
		m.recordStateTransition(ctx, string(StateStopped), string(StateRunning))
	}

	// Return instance with derived state (should be Running now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance started successfully", "id", id, "state", finalInst.State)
	return &finalInst, nil
}
