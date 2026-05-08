package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/attribute"
	"gvisor.dev/gvisor/pkg/cleanup"
)

// startInstance starts a stopped instance
// Transition: Stopped → Running
func (m *manager) startInstance(
	ctx context.Context,
	id string,
	req StartInstanceRequest,
) (_ *Instance, retErr error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "starting instance", "instance_id", id)

	ctx, span := m.startLifecycleSpan(ctx, "instances.start",
		attribute.String("instance_id", id),
		attribute.String("operation", "start"),
	)
	defer func() { finishInstancesSpan(span, retErr) }()

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "instance_id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	ctx = enrichInstancesTrace(ctx, attribute.String("hypervisor", string(stored.HypervisorType)))
	log.DebugContext(ctx, "loaded instance", "instance_id", id, "state", inst.State)

	// 2. Validate state (must be Stopped to start)
	if inst.State != StateStopped {
		log.ErrorContext(ctx, "invalid state for start", "instance_id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot start from state %s, must be Stopped", ErrInvalidState, inst.State)
	}
	if err := m.templateGuard(stored, "start"); err != nil {
		log.ErrorContext(ctx, "refusing to start template instance", "instance_id", id, "template_id", stored.TemplateID)
		return nil, err
	}

	// 2a. Clear stale exit info from previous run and apply command overrides
	stored.ExitCode = nil
	stored.ExitMessage = ""
	stored.ProgramStartedAt = nil
	stored.GuestAgentReadyAt = nil
	if len(req.Entrypoint) > 0 {
		stored.Entrypoint = req.Entrypoint
	}
	if len(req.Cmd) > 0 {
		stored.Cmd = req.Cmd
	}

	// 2b. Validate aggregate resource limits before allocating resources (if configured)
	reservedResources := false
	if m.resourceValidator != nil {
		needsGPU := stored.GPUProfile != ""
		totalMemory := stored.Size + stored.HotplugSize
		diskBytes := storedDiskReservationBytes(stored)
		if err := m.resourceValidator.ReserveAllocation(ctx, id, stored.Vcpus, totalMemory, stored.NetworkBandwidthDownload, stored.NetworkBandwidthUpload, stored.DiskIOBps, diskBytes, needsGPU); err != nil {
			log.ErrorContext(ctx, "resource reservation failed for start", "instance_id", id, "error", err)
			return nil, fmt.Errorf("%w: %v", ErrInsufficientResources, err)
		}
		reservedResources = true
		defer func() {
			if reservedResources {
				m.resourceValidator.FinishAllocation(id)
			}
		}()
	}

	// 3. Get image info (needed for buildHypervisorConfig)
	log.DebugContext(ctx, "getting image info", "instance_id", id, "image", stored.Image)
	imageCtx, imageSpanEnd := m.startLifecycleStep(ctx, "resolve_image",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "resolve_image"),
	)
	imageInfo, err := m.imageManager.GetImage(imageCtx, stored.Image)
	imageSpanEnd(err)
	if err != nil {
		log.ErrorContext(ctx, "failed to get image", "instance_id", id, "image", stored.Image, "error", err)
		return nil, fmt.Errorf("get image: %w", err)
	}

	// Setup cleanup stack for automatic rollback on errors
	cu := cleanup.Make(func() {})
	defer cu.Clean()

	// 4. Allocate fresh network if network enabled
	var netConfig *network.NetworkConfig
	if stored.NetworkEnabled {
		log.DebugContext(ctx, "allocating network for start", "instance_id", id, "network", "default")
		networkCtx, networkSpanEnd := m.startLifecycleStep(ctx, "allocate_network",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "allocate_network"),
			attribute.Bool("network_enabled", true),
		)
		netConfig, err = m.networkManager.CreateAllocation(networkCtx, network.AllocateRequest{
			InstanceID:   id,
			InstanceName: stored.Name,
		})
		networkSpanEnd(err)
		if err != nil {
			log.ErrorContext(ctx, "failed to allocate network", "instance_id", id, "error", err)
			return nil, fmt.Errorf("allocate network: %w", err)
		}
		// Update stored metadata with new IP/MAC
		stored.IP = netConfig.IP
		stored.MAC = netConfig.MAC
		// Add network cleanup to stack
		cu.Add(func() {
			m.networkManager.ReleaseAllocation(ctx, &network.Allocation{
				InstanceID: id,
				TAPDevice:  netConfig.TAPDevice,
			})
		})
	}

	var proxyGuestConfig *egressproxy.GuestConfig
	if stored.NetworkEnabled {
		proxyGuestConfig, err = m.maybeRegisterEgressProxy(ctx, stored, netConfig)
		if err != nil {
			log.ErrorContext(ctx, "failed to configure egress proxy", "instance_id", id, "error", err)
			return nil, fmt.Errorf("configure egress proxy: %w", err)
		}
		if proxyGuestConfig != nil {
			cu.Add(func() {
				m.unregisterEgressProxyInstance(ctx, id)
			})
		}
	}

	// 4b. Recreate vGPU mdev if this instance had a GPU profile
	// Note: GPU availability was already validated in step 2b
	if stored.GPUProfile != "" {
		log.InfoContext(ctx, "creating vGPU mdev for start", "instance_id", id, "profile", stored.GPUProfile)
		mdev, err := devices.CreateMdev(ctx, stored.GPUProfile, id)
		if err != nil {
			log.ErrorContext(ctx, "failed to create mdev", "instance_id", id, "profile", stored.GPUProfile, "error", err)
			return nil, fmt.Errorf("create vGPU mdev for profile %s: %w", stored.GPUProfile, err)
		}
		stored.GPUMdevUUID = mdev.UUID
		log.InfoContext(ctx, "created vGPU mdev", "instance_id", id, "profile", stored.GPUProfile, "uuid", mdev.UUID)
		// Add mdev cleanup to stack
		cu.Add(func() {
			log.DebugContext(ctx, "destroying mdev on cleanup", "instance_id", id, "uuid", mdev.UUID)
			if err := devices.DestroyMdev(ctx, mdev.UUID); err != nil {
				log.WarnContext(ctx, "failed to destroy mdev on cleanup", "instance_id", id, "uuid", mdev.UUID, "error", err)
			}
		})
	}

	// 5. Regenerate config disk with new network configuration
	instForConfig := &Instance{StoredMetadata: *stored}
	log.DebugContext(ctx, "regenerating config disk", "instance_id", id)
	configDiskCtx, configDiskSpanEnd := m.startLifecycleStep(ctx, "create_config_disk",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "create_config_disk"),
	)
	if err := m.createConfigDisk(configDiskCtx, instForConfig, imageInfo, netConfig, proxyGuestConfig); err != nil {
		configDiskSpanEnd(err)
		log.ErrorContext(ctx, "failed to create config disk", "instance_id", id, "error", err)
		return nil, fmt.Errorf("create config disk: %w", err)
	}
	configDiskSpanEnd(nil)

	if err := m.archiveAppLogForBoot(id); err != nil {
		log.WarnContext(ctx, "failed to archive app log before start", "instance_id", id, "error", err)
	}

	// 6. Start hypervisor and boot VM (reuses logic from create)
	bootStart := time.Now().UTC()
	stored.StartedAt = &bootStart

	log.InfoContext(ctx, "starting hypervisor and booting VM", "instance_id", id)
	startVMCtx, startVMSpanEnd := m.startLifecycleStep(ctx, "start_vm",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "start_vm"),
	)
	if err := m.startAndBootVM(startVMCtx, stored, imageInfo, netConfig); err != nil {
		startVMSpanEnd(err)
		log.ErrorContext(ctx, "failed to start and boot VM", "instance_id", id, "error", err)
		return nil, err
	}
	startVMSpanEnd(nil)
	// Mark the instance visible before releasing its pending reservation so we
	// never create an undercount window. The tiny overlap is intentionally
	// over-conservative: concurrent admissions may briefly see both visible and
	// pending usage for this instance, but they will not oversubscribe the host.
	m.setAdmissionAllocationActive(stored, true)
	if reservedResources {
		m.resourceValidator.FinishAllocation(id)
		reservedResources = false
	}

	// Success - release cleanup stack (prevent cleanup)
	cu.Release()

	// 7. Update metadata (set PID, StartedAt)
	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed - log but don't fail
		log.WarnContext(ctx, "failed to update metadata after VM start", "instance_id", id, "error", err)
	}

	// Return instance state from current metadata without forcing a log scan.
	finalInst := m.toInstanceWithoutHydration(ctx, meta)
	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.startDuration, start, "success", stored.HypervisorType)
		m.recordStateTransition(ctx, string(StateStopped), string(finalInst.State), stored.HypervisorType)
	}
	log.InfoContext(ctx, "instance started successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}
