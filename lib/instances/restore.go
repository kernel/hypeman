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
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/trace"
)

// RestoreInstance restores an instance from standby
// Multi-hop orchestration: Standby → Paused → Running
func (m *manager) restoreInstance(
	ctx context.Context,

	id string,
) (*Instance, error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "restoring instance from standby", "instance_id", id)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "RestoreInstance")
		defer span.End()
	}

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "instance_id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
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
	if m.resourceValidator != nil {
		needsGPU := stored.GPUProfile != ""
		totalMemory := stored.Size + stored.HotplugSize
		if err := m.resourceValidator.ValidateAllocation(ctx, stored.Vcpus, totalMemory, stored.NetworkBandwidthDownload, stored.NetworkBandwidthUpload, stored.DiskIOBps, needsGPU); err != nil {
			log.ErrorContext(ctx, "resource validation failed for restore", "instance_id", id, "error", err)
			return nil, fmt.Errorf("%w: %v", ErrInsufficientResources, err)
		}
	}

	// 3. Get snapshot directory
	snapshotDir := m.paths.InstanceSnapshotLatest(id)
	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return nil, fmt.Errorf("get vm starter: %w", err)
	}

	var allocatedNet *network.Allocation
	releaseNetwork := func() {
		if !stored.NetworkEnabled {
			return
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
		var networkSpan trace.Span
		if m.metrics != nil && m.metrics.tracer != nil {
			ctx, networkSpan = m.metrics.tracer.Start(ctx, "RestoreNetwork")
		}
		// If IP/MAC is empty (forked standby flow), allocate a fresh identity and
		// patch the copied snapshot config before restore.
		if stored.IP == "" || stored.MAC == "" {
			log.InfoContext(ctx, "allocating fresh network identity for standby restore",
				"instance_id", id, "network", "default",
				"download_bps", stored.NetworkBandwidthDownload, "upload_bps", stored.NetworkBandwidthUpload)
			netConfig, err := m.networkManager.CreateAllocation(ctx, network.AllocateRequest{
				InstanceID:    id,
				InstanceName:  stored.Name,
				DownloadBps:   stored.NetworkBandwidthDownload,
				UploadBps:     stored.NetworkBandwidthUpload,
				UploadCeilBps: stored.NetworkBandwidthUpload * int64(m.networkManager.GetUploadBurstMultiplier()),
			})
			if err != nil {
				if networkSpan != nil {
					networkSpan.End()
				}
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
			}
			stored.IP = netConfig.IP
			stored.MAC = netConfig.MAC

			if _, err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{
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
				if networkSpan != nil {
					networkSpan.End()
				}
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
			if err := m.networkManager.RecreateAllocation(ctx, id, stored.NetworkBandwidthDownload, stored.NetworkBandwidthUpload); err != nil {
				if networkSpan != nil {
					networkSpan.End()
				}
				log.ErrorContext(ctx, "failed to recreate network", "instance_id", id, "error", err)
				return nil, fmt.Errorf("recreate network: %w", err)
			}
		}
		if networkSpan != nil {
			networkSpan.End()
		}
	}

	// 5. Transition: Standby → Paused (start hypervisor + restore)
	var restoreSpan trace.Span
	if m.metrics != nil && m.metrics.tracer != nil {
		ctx, restoreSpan = m.metrics.tracer.Start(ctx, "RestoreFromSnapshot")
	}
	log.InfoContext(ctx, "restoring from snapshot", "instance_id", id, "snapshot_dir", snapshotDir, "hypervisor", stored.HypervisorType)
	pid, hv, err := m.restoreFromSnapshot(ctx, stored, snapshotDir)
	if restoreSpan != nil {
		restoreSpan.End()
	}
	if err != nil {
		log.ErrorContext(ctx, "failed to restore from snapshot", "instance_id", id, "error", err)
		// Cleanup network on failure
		releaseNetwork()
		return nil, err
	}

	// Store the PID for later cleanup
	stored.HypervisorPID = &pid

	// 6. Transition: Paused → Running (resume)
	var resumeSpan trace.Span
	if m.metrics != nil && m.metrics.tracer != nil {
		ctx, resumeSpan = m.metrics.tracer.Start(ctx, "ResumeVM")
	}
	log.InfoContext(ctx, "resuming VM", "instance_id", id)
	if err := hv.Resume(ctx); err != nil {
		if resumeSpan != nil {
			resumeSpan.End()
		}
		log.ErrorContext(ctx, "failed to resume VM", "instance_id", id, "error", err)
		// Cleanup on failure
		hv.Shutdown(ctx)
		releaseNetwork()
		return nil, fmt.Errorf("resume vm failed: %w", err)
	}
	if resumeSpan != nil {
		resumeSpan.End()
	}

	// Forked standby restores may allocate a fresh identity while the guest memory snapshot
	// still has the source VM's old IP configuration. Reconfigure guest networking after
	// resume so host ingress to the new private IP works reliably.
	if allocatedNet != nil && !stored.SkipGuestAgent {
		if err := reconfigureGuestNetwork(ctx, stored, allocatedNet); err != nil {
			log.ErrorContext(ctx, "failed to configure guest network after restore", "instance_id", id, "error", err)
			_ = hv.Shutdown(ctx)
			releaseNetwork()
			return nil, fmt.Errorf("configure guest network after restore: %w", err)
		}
	}

	// 8. Delete snapshot after successful restore
	log.InfoContext(ctx, "deleting snapshot after successful restore", "instance_id", id)
	os.RemoveAll(snapshotDir) // Best effort, ignore errors

	// 9. Update timestamp
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed
		log.WarnContext(ctx, "failed to update metadata after restore", "instance_id", id, "error", err)
	}

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.restoreDuration, start, "success", stored.HypervisorType)
		m.recordStateTransition(ctx, string(StateStandby), string(StateRunning), stored.HypervisorType)
	}

	// Return instance with derived state (should be Running now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance restored successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
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
