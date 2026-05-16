package instances

import (
	"context"
	"errors"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

const (
	runtimeOrphanGCInterval = 60 * time.Second
	runtimeOrphanMinAge     = 5 * time.Minute
)

type orphanRuntimeProcess struct {
	PID        int
	InstanceID string
	Age        time.Duration
	Command    string
}

// StartRuntimeOrphanReconciler adopts or removes hypervisor runtimes left behind
// after hypeman-api restarts. This protects hosts running systemd KillMode=process,
// where qemu/firecracker children can survive the API process and become PPID=1.
func (m *manager) StartRuntimeOrphanReconciler(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.runtimeOrphanGCOnce.Do(func() {
		go func() {
			m.reconcileRuntimeOrphans(ctx)
			ticker := time.NewTicker(runtimeOrphanGCInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.reconcileRuntimeOrphans(ctx)
				}
			}
		}()
	})
}

func (m *manager) reconcileRuntimeOrphans(ctx context.Context) {
	log := logger.FromContext(ctx)

	orphaned, err := scanOrphanRuntimeProcesses(m.paths.GuestsDir())
	if err != nil {
		log.WarnContext(ctx, "runtime orphan GC: failed to scan processes", "error", err)
		return
	}
	for _, proc := range orphaned {
		meta, err := m.loadMetadata(proc.InstanceID)
		if err == nil {
			if meta.HypervisorPID == nil || *meta.HypervisorPID != proc.PID {
				pid := proc.PID
				meta.HypervisorPID = &pid
				if saveErr := m.saveMetadata(meta); saveErr != nil {
					log.WarnContext(ctx, "runtime orphan GC: failed to adopt runtime process",
						"instance_id", proc.InstanceID,
						"pid", proc.PID,
						"error", saveErr,
					)
					continue
				}
				log.InfoContext(ctx, "runtime orphan GC: adopted runtime process",
					"instance_id", proc.InstanceID,
					"pid", proc.PID,
				)
			}
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			log.WarnContext(ctx, "runtime orphan GC: failed to load metadata",
				"instance_id", proc.InstanceID,
				"pid", proc.PID,
				"error", err,
			)
			continue
		}
		if proc.Age < runtimeOrphanMinAge {
			continue
		}
		if err := terminateRuntimeProcess(proc.PID); err != nil {
			log.WarnContext(ctx, "runtime orphan GC: failed to terminate unowned runtime process",
				"instance_id", proc.InstanceID,
				"pid", proc.PID,
				"age", proc.Age,
				"error", err,
			)
			continue
		}
		log.InfoContext(ctx, "runtime orphan GC: terminated unowned runtime process",
			"instance_id", proc.InstanceID,
			"pid", proc.PID,
			"age", proc.Age,
		)
	}
}
