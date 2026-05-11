package instances

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"go.opentelemetry.io/otel/attribute"
)

// RestoreInstance restores an instance from standby
// Multi-hop orchestration: Standby → Paused → Running
func (m *manager) restoreInstance(
	ctx context.Context,

	id string,
) (_ *Instance, retErr error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "restoring instance from standby", "instance_id", id)

	ctx, span := m.startLifecycleSpan(ctx, "instances.restore",
		attribute.String("instance_id", id),
		attribute.String("operation", "restore"),
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
	log.DebugContext(ctx, "loaded instance", "instance_id", id, "state", inst.State, "has_snapshot", inst.HasSnapshot)

	// 2. Validate state
	if inst.State != StateStandby {
		log.ErrorContext(ctx, "invalid state for restore", "instance_id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot restore from state %s", ErrInvalidState, inst.State)
	}

	if !inst.HasSnapshot {
		log.ErrorContext(ctx, "no snapshot available", "instance_id", id)
		return nil, fmt.Errorf("no snapshot available for instance %s", id)
	}

	// 2b. Validate aggregate resource limits before allocating resources (if configured)
	reservedResources := false
	if m.resourceValidator != nil {
		needsGPU := stored.GPUProfile != ""
		totalMemory := stored.Size + stored.HotplugSize
		diskBytes := storedDiskReservationBytes(stored)
		if err := m.resourceValidator.ReserveAllocation(ctx, id, stored.Vcpus, totalMemory, stored.NetworkBandwidthDownload, stored.NetworkBandwidthUpload, stored.DiskIOBps, diskBytes, needsGPU); err != nil {
			log.ErrorContext(ctx, "resource reservation failed for restore", "instance_id", id, "error", err)
			return nil, fmt.Errorf("%w: %v", ErrInsufficientResources, err)
		}
		reservedResources = true
		defer func() {
			if reservedResources {
				m.resourceValidator.FinishAllocation(id)
			}
		}()
	}

	// 3. Get snapshot directory
	snapshotDir := m.paths.InstanceSnapshotLatest(id)
	restoreCompression := m.restoreCompressionConfigForMetrics(stored, snapshotDir)
	prepareSnapshotCtx, prepareSnapshotSpanEnd := m.startLifecycleStep(ctx, "prepare_snapshot_memory",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "prepare_snapshot_memory"),
	)
	err = m.ensureSnapshotMemoryReady(prepareSnapshotCtx, snapshotDir, m.snapshotJobKeyForInstance(id), stored.HypervisorType)
	prepareSnapshotSpanEnd(err)
	if err != nil {
		return nil, fmt.Errorf("prepare standby snapshot memory: %w", err)
	}
	stored.PendingStandbyCompression = nil
	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return nil, fmt.Errorf("get vm starter: %w", err)
	}

	var allocatedNet *network.Allocation
	proxyRegistered := false
	releaseNetwork := func() {
		if !stored.NetworkEnabled {
			return
		}
		if proxyRegistered {
			m.unregisterEgressProxyInstance(ctx, id)
			proxyRegistered = false
		}
		if allocatedNet != nil {
			if err := m.networkManager.ReleaseAllocation(ctx, allocatedNet); err != nil {
				log.WarnContext(ctx, "failed to release allocated network", "instance_id", id, "error", err)
			}
			return
		}
		netAlloc, _ := m.networkManager.GetAllocation(ctx, id)
		if err := m.networkManager.ReleaseAllocation(ctx, netAlloc); err != nil {
			log.WarnContext(ctx, "failed to release network", "instance_id", id, "error", err)
		}
	}

	// 4. Recreate or allocate network if network enabled
	if stored.NetworkEnabled {
		networkCtx, networkSpanEnd := m.startLifecycleStep(ctx, "restore_network",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "restore_network"),
			attribute.Bool("network_enabled", true),
		)
		// If IP/MAC is empty (forked standby flow), allocate a fresh identity and
		// patch the copied snapshot config before restore.
		if stored.IP == "" || stored.MAC == "" {
			log.InfoContext(ctx, "allocating fresh network identity for standby restore",
				"instance_id", id, "network", "default",
				"download_bps", stored.NetworkBandwidthDownload, "upload_bps", stored.NetworkBandwidthUpload)
			netConfig, err := m.networkManager.CreateAllocation(networkCtx, network.AllocateRequest{
				InstanceID:    id,
				InstanceName:  stored.Name,
				DownloadBps:   stored.NetworkBandwidthDownload,
				UploadBps:     stored.NetworkBandwidthUpload,
				UploadCeilBps: stored.NetworkBandwidthUpload * int64(m.networkManager.GetUploadBurstMultiplier()),
			})
			if err != nil {
				networkSpanEnd(err)
				log.ErrorContext(ctx, "failed to allocate network", "instance_id", id, "error", err)
				return nil, fmt.Errorf("allocate network: %w", err)
			}
			allocatedNet = &network.Allocation{
				InstanceID:   id,
				InstanceName: stored.Name,
				Network:      "default",
				IP:           netConfig.IP,
				MAC:          netConfig.MAC,
				TAPDevice:    netConfig.TAPDevice,
				Gateway:      netConfig.Gateway,
				Netmask:      netConfig.Netmask,
				DNS:          netConfig.DNS,
			}
			stored.IP = netConfig.IP
			stored.MAC = netConfig.MAC

			if _, err := starter.PrepareFork(networkCtx, hypervisor.ForkPrepareRequest{
				SnapshotConfigPath: m.paths.InstanceSnapshotConfig(id),
				VsockCID:           stored.VsockCID,
				VsockSocket:        stored.VsockSocket,
				Network: &hypervisor.ForkNetworkConfig{
					TAPDevice: netConfig.TAPDevice,
					IP:        netConfig.IP,
					MAC:       netConfig.MAC,
					Netmask:   netConfig.Netmask,
				},
			}); err != nil {
				networkSpanEnd(err)
				if errors.Is(err, hypervisor.ErrNotSupported) {
					log.ErrorContext(ctx, "forked standby network rewrite not supported for hypervisor", "instance_id", id, "hypervisor", stored.HypervisorType)
					releaseNetwork()
					return nil, fmt.Errorf("%w: standby fork restore network rewrite is not supported for hypervisor %s", ErrNotSupported, stored.HypervisorType)
				}
				log.ErrorContext(ctx, "failed to patch snapshot network identity", "instance_id", id, "error", err)
				releaseNetwork()
				return nil, fmt.Errorf("rewrite snapshot config: %w", err)
			}
		} else {
			log.InfoContext(ctx, "recreating network for restore", "instance_id", id, "network", "default",
				"download_bps", stored.NetworkBandwidthDownload, "upload_bps", stored.NetworkBandwidthUpload)
			if err := m.networkManager.RecreateAllocation(networkCtx, id, stored.NetworkBandwidthDownload, stored.NetworkBandwidthUpload); err != nil {
				networkSpanEnd(err)
				log.ErrorContext(ctx, "failed to recreate network", "instance_id", id, "error", err)
				return nil, fmt.Errorf("recreate network: %w", err)
			}
		}
		networkSpanEnd(nil)
	}

	// 4b. Register proxy/enforcement once network identity is active.
	// Restore config disk refresh is only required for instances using network egress mediation.
	if requiresRestoreConfigDiskRefresh(stored) {
		alloc, allocErr := m.networkManager.GetAllocation(ctx, id)
		if allocErr != nil {
			log.ErrorContext(ctx, "failed to fetch allocation for proxy setup", "instance_id", id, "error", allocErr)
			releaseNetwork()
			return nil, fmt.Errorf("get network allocation for proxy setup: %w", allocErr)
		}
		if alloc == nil {
			log.ErrorContext(ctx, "missing allocation for proxy setup", "instance_id", id)
			releaseNetwork()
			return nil, fmt.Errorf("get network allocation for proxy setup: allocation not found")
		}

		proxyCfg := networkConfigFromAllocation(alloc)
		proxyGuestConfig, err := m.maybeRegisterEgressProxy(ctx, stored, proxyCfg)
		if err != nil {
			log.ErrorContext(ctx, "failed to configure egress proxy", "instance_id", id, "error", err)
			releaseNetwork()
			return nil, fmt.Errorf("configure egress proxy: %w", err)
		}
		imageInfo, err := m.imageManager.GetImage(ctx, stored.Image)
		if err != nil {
			log.ErrorContext(ctx, "failed to load image for config disk refresh", "instance_id", id, "image", stored.Image, "error", err)
			releaseNetwork()
			return nil, fmt.Errorf("get image for restore config disk: %w", err)
		}
		instForConfig := &Instance{StoredMetadata: *stored}
		if err := m.createConfigDisk(ctx, instForConfig, imageInfo, proxyCfg, proxyGuestConfig); err != nil {
			log.ErrorContext(ctx, "failed to refresh config disk for restore", "instance_id", id, "error", err)
			releaseNetwork()
			return nil, fmt.Errorf("refresh restore config disk: %w", err)
		}
		proxyRegistered = true
	}

	// 5. Transition: Standby → Paused (start hypervisor + restore)
	restoreCtx, restoreSpanEnd := m.startLifecycleStep(ctx, "restore_from_snapshot",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "restore_from_snapshot"),
	)
	log.InfoContext(ctx, "restoring from snapshot", "instance_id", id, "snapshot_dir", snapshotDir, "hypervisor", stored.HypervisorType)
	pid, hv, err := m.restoreFromSnapshot(restoreCtx, stored, snapshotDir)
	restoreSpanEnd(err)
	if err != nil {
		log.ErrorContext(ctx, "failed to restore from snapshot", "instance_id", id, "error", err)
		// Cleanup network on failure
		releaseNetwork()
		return nil, err
	}

	// Store the PID for later cleanup
	stored.HypervisorPID = &pid

	// 6. Transition: Paused → Running (resume)
	resumeCtx, resumeSpanEnd := m.startLifecycleStep(ctx, "resume_vm",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "resume_vm"),
	)
	log.InfoContext(ctx, "resuming VM", "instance_id", id)
	if err := hv.Resume(resumeCtx); err != nil {
		resumeSpanEnd(err)
		log.ErrorContext(ctx, "failed to resume VM", "instance_id", id, "error", err)
		// Cleanup on failure
		hv.Shutdown(ctx)
		releaseNetwork()
		return nil, fmt.Errorf("resume vm failed: %w", err)
	}
	resumeSpanEnd(nil)
	// Mark the instance visible before releasing its pending reservation so we
	// never create an undercount window. The tiny overlap is intentionally
	// over-conservative: concurrent admissions may briefly see both visible and
	// pending usage for this instance, but they will not oversubscribe the host.
	m.setAdmissionAllocationActive(stored, true)
	if reservedResources {
		m.resourceValidator.FinishAllocation(id)
		reservedResources = false
	}

	// Forked standby restores may allocate a fresh identity while the guest memory snapshot
	// still has the source VM's old IP configuration. Reconfigure guest networking after
	// resume so host ingress to the new private IP works reliably.
	if allocatedNet != nil && !stored.SkipGuestAgent {
		reconfigureCtx, reconfigureSpanEnd := m.startLifecycleStep(ctx, "reconfigure_guest_network",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "reconfigure_guest_network"),
		)
		if err := reconfigureGuestNetwork(reconfigureCtx, stored, allocatedNet); err != nil {
			reconfigureSpanEnd(err)
			log.ErrorContext(ctx, "failed to configure guest network after restore", "instance_id", id, "error", err)
			_ = hv.Shutdown(ctx)
			m.rollbackAdmissionAllocationActive(stored)
			releaseNetwork()
			return nil, fmt.Errorf("configure guest network after restore: %w", err)
		}
		reconfigureSpanEnd(nil)
	}

	// 8. Delete snapshot after successful restore unless the hypervisor is keeping it
	// as the base for the next standby snapshot.
	if m.supportsSnapshotBaseReuse(stored.HypervisorType) {
		retainedBaseDir := m.paths.InstanceSnapshotBase(id)
		if err := restoreRetainedSnapshotBase(snapshotDir, retainedBaseDir); err != nil {
			log.WarnContext(ctx, "failed to retain snapshot base after restore", "instance_id", id, "error", err)
		}
	} else {
		log.InfoContext(ctx, "deleting snapshot after successful restore", "instance_id", id)
		os.RemoveAll(snapshotDir) // Best effort, ignore errors
	}

	// 9. Persist runtime metadata updates without resetting StartedAt.
	// Restore resumes an existing boot; preserving StartedAt keeps marker
	// hydration scoped to the original boot timeline.
	stored.Phases.Record(phasetracking.PhaseRunning, time.Now().UTC())
	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed
		log.WarnContext(ctx, "failed to update metadata after restore", "instance_id", id, "error", err)
	}

	// Return instance state from current metadata without forcing a log scan.
	finalInst := m.toInstanceWithoutHydration(ctx, meta)
	// Record metrics
	if m.metrics != nil {
		m.recordDurationWithCompression(ctx, m.metrics.restoreDuration, start, "success", stored.HypervisorType, restoreCompression)
		m.recordStateTransition(ctx, string(StateStandby), string(finalInst.State), stored.HypervisorType)
	}
	log.InfoContext(ctx, "instance restored successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}

func (m *manager) restoreCompressionConfigForMetrics(stored *StoredMetadata, snapshotDir string) *snapshotstore.SnapshotCompressionConfig {
	_, algorithm, ok := findCompressedSnapshotMemoryFile(snapshotDir)
	if !ok {
		return nil
	}

	cfg := snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: algorithm,
	}
	if stored == nil {
		return &cfg
	}

	policy, err := m.resolveSnapshotCompressionPolicy(stored, nil)
	if err != nil || !policy.Enabled {
		return &cfg
	}

	resolved := compressionMetadataForExistingArtifact(policy, algorithm)
	return &resolved
}

// restoreFromSnapshot starts the hypervisor and restores from snapshot
func (m *manager) restoreFromSnapshot(
	ctx context.Context,
	stored *StoredMetadata,
	snapshotDir string,
) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	// Get VM starter for this hypervisor type
	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return 0, nil, fmt.Errorf("get vm starter: %w", err)
	}

	// Restore VM from snapshot (handles process start + restore)
	log.DebugContext(ctx, "restoring VM from snapshot", "instance_id", stored.Id, "hypervisor", stored.HypervisorType, "version", stored.HypervisorVersion, "snapshot_dir", snapshotDir)
	pid, hv, err := starter.RestoreVM(ctx, m.paths, stored.HypervisorVersion, stored.SocketPath, snapshotDir)
	if err != nil {
		return 0, nil, fmt.Errorf("restore vm: %w", err)
	}
	pid = resolveRuntimeHypervisorPID(log, stored.SocketPath, pid)

	log.DebugContext(ctx, "VM restored from snapshot successfully", "instance_id", stored.Id, "pid", pid)
	return pid, hv, nil
}

func reconfigureGuestNetwork(ctx context.Context, stored *StoredMetadata, alloc *network.Allocation) error {
	prefix, err := netmaskToPrefix(alloc.Netmask)
	if err != nil {
		return err
	}

	dialer, err := hypervisor.NewVsockDialer(stored.HypervisorType, stored.VsockSocket, stored.VsockCID)
	if err != nil {
		return fmt.Errorf("create vsock dialer: %w", err)
	}

	cmd := fmt.Sprintf(
		"ip -4 addr flush dev eth0 scope global && ip addr add %s/%d dev eth0 && ip link set dev eth0 up && ip route replace default via %s dev eth0",
		alloc.IP, prefix, alloc.Gateway,
	)

	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      []string{"sh", "-c", cmd},
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitForAgent: 120 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("exec network reconfiguration command: %w", err)
	}
	if exit.Code != 0 {
		return fmt.Errorf("network reconfiguration command failed (exit=%d, stdout=%q, stderr=%q)", exit.Code, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
	}

	return nil
}

func networkConfigFromAllocation(alloc *network.Allocation) *network.NetworkConfig {
	if alloc == nil {
		return nil
	}
	return &network.NetworkConfig{
		IP:        alloc.IP,
		MAC:       alloc.MAC,
		Gateway:   alloc.Gateway,
		Netmask:   alloc.Netmask,
		DNS:       alloc.DNS,
		TAPDevice: alloc.TAPDevice,
	}
}

func requiresRestoreConfigDiskRefresh(stored *StoredMetadata) bool {
	return stored != nil && stored.NetworkEnabled && stored.NetworkEgress != nil && stored.NetworkEgress.Enabled
}

func netmaskToPrefix(mask string) (int, error) {
	ip := net.ParseIP(mask).To4()
	if ip == nil {
		return 0, fmt.Errorf("invalid netmask: %q", mask)
	}
	ones, bits := net.IPMask(ip).Size()
	if bits != 32 {
		return 0, fmt.Errorf("invalid netmask bits: %q", mask)
	}
	return ones, nil
}
