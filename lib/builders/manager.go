package builders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/nrednav/cuid2"
	"go.opentelemetry.io/otel/metric"
)

// Manager provides builder lifecycle operations
type Manager interface {
	// Start reconciles on-disk state after a restart and starts the idle
	// reaper when configured. Called once when the API server starts.
	Start(ctx context.Context) error

	// CreateBuilder creates a builder and eagerly provisions its disk
	CreateBuilder(ctx context.Context, req CreateBuilderRequest) (*Builder, error)

	// GetBuilder returns a builder by ID
	GetBuilder(ctx context.Context, id string) (*Builder, error)

	// ListBuilders returns all builders
	ListBuilders(ctx context.Context) ([]Builder, error)

	// DeleteBuilder deletes a builder and its disk. Deletion is total and
	// irreversible. Returns ErrInUse while the builder is acquired by a
	// build, has its disk attached, or is mid-prune.
	DeleteBuilder(ctx context.Context, id string) error

	// AcquireForBuild marks a builder as held by a build, recreating a
	// missing disk as best-effort cache recovery. Returns ErrInUse when the
	// builder is already held or not ready. A disk attachment on an unheld,
	// ready builder can only be a leaked record or a stale VM from a
	// crashed build, so it is cleared before the acquisition instead of
	// failing builds until the next restart reconciliation.
	AcquireForBuild(ctx context.Context, id string, buildID string) (*Builder, error)

	// ReleaseBuild releases a builder held by AcquireForBuild and records
	// last_used_at, including for failed builds.
	ReleaseBuild(ctx context.Context, id string, buildID string) error

	// ResetDisk resets a builder's cache by recreating its disk
	// asynchronously: the returned snapshot has status pruning, followed by
	// ready (or error). Returns ErrInUse while the builder is acquired by a
	// build, has its disk attached, or is mid-delete.
	ResetDisk(ctx context.Context, id string) (*Builder, error)

	// SetBuildActivityChecker registers a function reporting whether a
	// builder has queued or running builds. DeleteBuilder and ResetDisk
	// consult it so they also return ErrInUse while builds are queued,
	// before any build holds the acquisition.
	SetBuildActivityChecker(fn func(builderID string) bool)
}

// instanceChecker is the subset of the instance manager reconciliation
// needs to clear stale disk attachments.
type instanceChecker interface {
	GetInstance(ctx context.Context, idOrName string) (*instances.Instance, error)
	DeleteInstance(ctx context.Context, id string) error
}

// builderIDPattern constrains caller-supplied builder IDs to characters
// that are safe in volume IDs and filesystem paths.
var builderIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidateBuilderID validates a caller-supplied builder ID.
func ValidateBuilderID(id string) error {
	if !builderIDPattern.MatchString(id) {
		return fmt.Errorf("%w: must match %s", ErrInvalidID, builderIDPattern)
	}
	return nil
}

type manager struct {
	config        Config
	paths         *paths.Paths
	volumeManager volumes.Manager
	instanceMgr   instanceChecker
	logger        *slog.Logger
	metrics       *Metrics

	mu       sync.Mutex
	acquired map[string]string // builder ID -> build ID holding it

	hasBuilds func(builderID string) bool
}

// NewManager creates a new builders manager.
// If meter is nil, metrics are disabled.
func NewManager(
	p *paths.Paths,
	config Config,
	volumeMgr volumes.Manager,
	instanceMgr instanceChecker,
	logger *slog.Logger,
	meter metric.Meter,
) (Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if config.DefaultDiskSizeGb <= 0 {
		config.DefaultDiskSizeGb = DefaultDiskSizeGb
	}

	m := &manager{
		config:        config,
		paths:         p,
		volumeManager: volumeMgr,
		instanceMgr:   instanceMgr,
		logger:        logger,
		acquired:      make(map[string]string),
	}

	if meter != nil {
		metrics, err := newBuilderMetrics(meter, m)
		if err != nil {
			return nil, fmt.Errorf("create metrics: %w", err)
		}
		m.metrics = metrics
	}

	return m, nil
}

// Start reconciles on-disk state after a restart and starts the idle reaper
func (m *manager) Start(ctx context.Context) error {
	if err := m.reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile builders: %w", err)
	}
	if m.config.IdleTTL > 0 {
		go m.runIdleReaper(ctx)
	}
	m.logger.Info("builders manager started")
	return nil
}

// SetBuildActivityChecker registers the queued/running build checker.
func (m *manager) SetBuildActivityChecker(fn func(builderID string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hasBuilds = fn
}

// buildsInFlightLocked reports whether the builder has queued or running
// builds. m.mu must be held.
func (m *manager) buildsInFlightLocked(id string) bool {
	return m.hasBuilds != nil && m.hasBuilds(id)
}

// CreateBuilder creates a builder and eagerly provisions its disk
func (m *manager) CreateBuilder(ctx context.Context, req CreateBuilderRequest) (*Builder, error) {
	start := time.Now()
	result := "error"
	defer func() { m.recordCreateDuration(ctx, start, result) }()

	if err := tags.Validate(req.Tags); err != nil {
		return nil, err
	}

	id := cuid2.Generate()
	if req.ID != nil && *req.ID != "" {
		if err := ValidateBuilderID(*req.ID); err != nil {
			return nil, err
		}
		id = *req.ID
	}

	sizeGb := req.DiskSizeGb
	if sizeGb < 0 {
		return nil, fmt.Errorf("%w: disk_size_gb must not be negative", ErrInvalidDiskSize)
	}
	if sizeGb == 0 {
		sizeGb = m.config.DefaultDiskSizeGb
	}
	if m.config.MaxDiskSizeGb > 0 && sizeGb > m.config.MaxDiskSizeGb {
		return nil, fmt.Errorf("%w: disk_size_gb %d exceeds maximum of %d", ErrDiskSizeExceeded, sizeGb, m.config.MaxDiskSizeGb)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := loadMetadata(m.paths, id); err == nil {
		return nil, ErrAlreadyExists
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	if m.config.MaxCount > 0 {
		ids, err := listBuilderIDs(m.paths)
		if err != nil {
			return nil, err
		}
		if len(ids) >= m.config.MaxCount {
			return nil, fmt.Errorf("%w: max_count %d", ErrQuotaExceeded, m.config.MaxCount)
		}
	}

	// Metadata first: a crash before the disk exists is a missing disk,
	// which startup reconciliation recreates. The reverse order would
	// orphan a reserved volume with no owning metadata.
	meta := &storedMetadata{
		ID:         id,
		Name:       req.Name,
		DiskSizeGb: sizeGb,
		Tags:       tags.Clone(req.Tags),
		Status:     StatusReady,
		CreatedAt:  time.Now(),
	}
	if err := saveMetadata(m.paths, meta); err != nil {
		return nil, err
	}

	if err := m.createDisk(ctx, meta); err != nil {
		// Keep ownership metadata until any partially-created reserved volume
		// is gone. Otherwise the volume becomes invisible to reconciliation
		// and a same-ID retry cannot recover it.
		meta.Status = StatusError
		if saveErr := saveMetadata(m.paths, meta); saveErr != nil {
			return nil, errors.Join(err, fmt.Errorf("persist builder create failure: %w", saveErr))
		}
		cleanupErr := m.volumeManager.DeleteVolume(ctx, meta.diskVolumeID())
		if cleanupErr != nil && !errors.Is(cleanupErr, volumes.ErrNotFound) {
			return nil, errors.Join(err, fmt.Errorf("clean up builder disk: %w", cleanupErr))
		}
		if cleanupErr := deleteBuilderData(m.paths, id); cleanupErr != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		return nil, err
	}

	result = "success"
	return meta.toBuilder(), nil
}

// createDisk provisions the builder's ext4 disk volume
func (m *manager) createDisk(ctx context.Context, meta *storedMetadata) error {
	volID := meta.diskVolumeID()
	_, err := m.volumeManager.CreateVolume(ctx, volumes.CreateVolumeRequest{
		Id:     &volID,
		Name:   volID,
		SizeGb: meta.DiskSizeGb,
		Tags:   tags.Tags{volumes.SystemTagNamespace + "managed-by": managedByTagValue},
	})
	if err != nil {
		return fmt.Errorf("create builder disk: %w", err)
	}
	return nil
}

// GetBuilder returns a builder by ID
func (m *manager) GetBuilder(ctx context.Context, id string) (*Builder, error) {
	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}
	return meta.toBuilder(), nil
}

// ListBuilders returns all builders
func (m *manager) ListBuilders(ctx context.Context) ([]Builder, error) {
	ids, err := listBuilderIDs(m.paths)
	if err != nil {
		return nil, err
	}

	builders := make([]Builder, 0, len(ids))
	for _, id := range ids {
		meta, err := loadMetadata(m.paths, id)
		if err != nil {
			return nil, fmt.Errorf("load builder %s: %w", id, err)
		}
		builders = append(builders, *meta.toBuilder())
	}
	return builders, nil
}

// DeleteBuilder deletes a builder and its disk
func (m *manager) DeleteBuilder(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return err
	}

	if meta.Status != StatusDeleting {
		if _, held := m.acquired[id]; held {
			return ErrInUse
		}
		if meta.Status == StatusPruning {
			return ErrInUse
		}
		if m.buildsInFlightLocked(id) {
			return ErrInUse
		}
		attached, err := m.diskAttached(ctx, meta.diskVolumeID())
		if err != nil {
			return err
		}
		if attached {
			return ErrInUse
		}

		// Persist the transition before any side effect so a crash
		// mid-delete is resumed by reconciliation.
		meta.Status = StatusDeleting
		if err := saveMetadata(m.paths, meta); err != nil {
			return err
		}
	}

	if err := m.volumeManager.DeleteVolume(ctx, meta.diskVolumeID()); err != nil && !errors.Is(err, volumes.ErrNotFound) {
		return fmt.Errorf("delete builder disk: %w", err)
	}

	return deleteBuilderData(m.paths, id)
}

// AcquireForBuild marks a builder as held by a build
func (m *manager) AcquireForBuild(ctx context.Context, id string, buildID string) (*Builder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}
	if meta.Status != StatusReady {
		return nil, ErrInUse
	}
	if _, held := m.acquired[id]; held {
		return nil, ErrInUse
	}

	// Best-effort cache recovery: a missing disk is recreated empty rather
	// than failing the build.
	if err := m.ensureDisk(ctx, meta); err != nil {
		return nil, err
	}

	attached, err := m.diskAttached(ctx, meta.diskVolumeID())
	if err != nil {
		return nil, err
	}
	if attached {
		// The builder is ready and unheld, so no live build can own an
		// attachment: it is a leaked record or a stale VM from a crashed
		// build. Clear it rather than failing every build until the next
		// restart reconciliation.
		if err := m.clearStaleAttachments(ctx, meta.diskVolumeID()); err != nil {
			return nil, fmt.Errorf("clear stale builder disk attachments: %w", err)
		}
	}

	m.acquired[id] = buildID
	return meta.toBuilder(), nil
}

// ReleaseBuild releases a builder held by AcquireForBuild
func (m *manager) ReleaseBuild(ctx context.Context, id string, buildID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	holder, held := m.acquired[id]
	if !held {
		return fmt.Errorf("builder %s is not acquired", id)
	}
	if holder != buildID {
		return fmt.Errorf("builder %s is acquired by build %s, not %s", id, holder, buildID)
	}

	// Record usage even for failed builds: last_used_at drives idle TTL.
	// Persist before releasing the hold so a failed call leaves the
	// builder acquired and the caller can retry.
	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			delete(m.acquired, id)
			return nil // builder was deleted while held
		}
		return err
	}
	now := time.Now()
	meta.LastUsedAt = &now
	if err := saveMetadata(m.paths, meta); err != nil {
		return err
	}
	delete(m.acquired, id)
	return nil
}

// ResetDisk resets a builder's cache by recreating its disk asynchronously
func (m *manager) ResetDisk(ctx context.Context, id string) (*Builder, error) {
	m.mu.Lock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if _, held := m.acquired[id]; held {
		m.mu.Unlock()
		return nil, ErrInUse
	}
	if meta.Status != StatusReady && meta.Status != StatusError {
		m.mu.Unlock()
		return nil, ErrInUse
	}
	if m.buildsInFlightLocked(id) {
		m.mu.Unlock()
		return nil, ErrInUse
	}
	attached, err := m.diskAttached(ctx, meta.diskVolumeID())
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if attached {
		m.mu.Unlock()
		return nil, ErrInUse
	}

	// Persist the transition before any side effect so a crash mid-prune
	// is resumed by reconciliation.
	meta.Status = StatusPruning
	if err := saveMetadata(m.paths, meta); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	accepted := meta.toBuilder()
	m.mu.Unlock()

	go func() {
		if err := m.resetDisk(context.Background(), id); err != nil {
			m.logger.Error("builder disk reset failed", "id", id, "error", err)
		}
	}()
	return accepted, nil
}

// resetDisk recreates a builder's disk and transitions it back to ready,
// or to error when the reset fails. The volume manager rejects deletion of
// an attached volume, so the pruning state is never left with a new disk
// that a stale VM could write to.
func (m *manager) resetDisk(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return err
	}

	fail := func(err error) error {
		meta.Status = StatusError
		if saveErr := saveMetadata(m.paths, meta); saveErr != nil {
			m.logger.Error("failed to persist builder error status", "id", id, "error", saveErr)
		}
		return err
	}

	if err := m.volumeManager.DeleteVolume(ctx, meta.diskVolumeID()); err != nil && !errors.Is(err, volumes.ErrNotFound) {
		return fail(fmt.Errorf("delete builder disk: %w", err))
	}
	if err := m.createDisk(ctx, meta); err != nil {
		return fail(err)
	}

	meta.Status = StatusReady
	return saveMetadata(m.paths, meta)
}

// ensureDisk recreates the builder's disk when it is missing. Returns nil
// when the disk already exists.
func (m *manager) ensureDisk(ctx context.Context, meta *storedMetadata) error {
	_, err := m.volumeManager.GetVolume(ctx, meta.diskVolumeID())
	if err == nil {
		return nil
	}
	if !errors.Is(err, volumes.ErrNotFound) {
		return fmt.Errorf("get builder disk: %w", err)
	}
	m.logger.Warn("builder disk missing, recreating empty", "id", meta.ID, "volume", meta.diskVolumeID())
	if err := m.createDisk(ctx, meta); err != nil && !errors.Is(err, volumes.ErrAlreadyExists) {
		return err
	}
	return nil
}

// diskAttached reports whether the builder's disk has any attachments
func (m *manager) diskAttached(ctx context.Context, volID string) (bool, error) {
	vol, err := m.volumeManager.GetVolume(ctx, volID)
	if errors.Is(err, volumes.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get builder disk: %w", err)
	}
	return len(vol.Attachments) > 0, nil
}

// reconcile restores consistency after a restart: interrupted deletes are
// finished, interrupted prunes are re-run, missing disks are recreated, and
// attachments no build can hold are removed.
func (m *manager) reconcile(ctx context.Context) error {
	ids, err := listBuilderIDs(m.paths)
	if err != nil {
		return err
	}

	var reconcileErrs []error
	for _, id := range ids {
		meta, err := loadMetadata(m.paths, id)
		if err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("load builder %s: %w", id, err))
			continue
		}

		switch meta.Status {
		case StatusDeleting:
			m.logger.Info("resuming interrupted builder delete", "id", id)
			if err := m.clearStaleAttachments(ctx, meta.diskVolumeID()); err != nil {
				reconcileErrs = append(reconcileErrs, fmt.Errorf("clear builder %s attachments before delete: %w", id, err))
				continue
			}
			if err := m.DeleteBuilder(ctx, id); err != nil {
				reconcileErrs = append(reconcileErrs, fmt.Errorf("resume builder %s delete: %w", id, err))
			}
			continue
		case StatusPruning:
			m.logger.Info("resuming interrupted builder prune", "id", id)
			if err := m.resetDisk(ctx, id); err != nil {
				reconcileErrs = append(reconcileErrs, fmt.Errorf("resume builder %s prune: %w", id, err))
			}
			continue
		}

		if err := m.ensureDisk(ctx, meta); err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("recreate builder %s disk: %w", id, err))
			continue
		}
		if err := m.clearStaleAttachments(ctx, meta.diskVolumeID()); err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("clear builder %s stale attachments: %w", id, err))
		}
	}
	return errors.Join(reconcileErrs...)
}

// clearStaleAttachments removes attachment records from a builder disk that
// no running build can own: records whose instance is gone are detached
// directly; records backed by a surviving (stale) VM are cleared by
// deleting that VM, then detaching any records that survive the deletion.
// No build holds an acquisition at startup, so every attachment is stale.
func (m *manager) clearStaleAttachments(ctx context.Context, volID string) error {
	vol, err := m.volumeManager.GetVolume(ctx, volID)
	if errors.Is(err, volumes.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get builder disk: %w", err)
	}

	deletedInstance := false
	for _, att := range vol.Attachments {
		_, err := m.instanceMgr.GetInstance(ctx, att.InstanceID)
		if err == nil {
			if delErr := m.instanceMgr.DeleteInstance(ctx, att.InstanceID); delErr != nil && !errors.Is(delErr, instances.ErrNotFound) {
				return fmt.Errorf("delete stale instance %s holding builder disk: %w", att.InstanceID, delErr)
			}
			deletedInstance = true
			m.logger.Warn("deleted stale instance holding builder disk", "instance", att.InstanceID, "volume", volID)
			continue
		}
		if !errors.Is(err, instances.ErrNotFound) {
			return fmt.Errorf("inspect instance %s holding builder disk: %w", att.InstanceID, err)
		}
		if detachErr := m.volumeManager.DetachVolume(ctx, volID, att.InstanceID); detachErr != nil {
			return fmt.Errorf("detach orphan attachment %s from builder disk: %w", att.InstanceID, detachErr)
		}
		m.logger.Warn("detached orphan builder disk attachment", "instance", att.InstanceID, "volume", volID)
	}

	// DeleteInstance only warns when a volume detach fails, so records can
	// survive the deletion; re-fetch and detach them.
	if deletedInstance {
		vol, err := m.volumeManager.GetVolume(ctx, volID)
		if errors.Is(err, volumes.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("re-fetch builder disk after stale instance deletion: %w", err)
		}
		for _, att := range vol.Attachments {
			if detachErr := m.volumeManager.DetachVolume(ctx, volID, att.InstanceID); detachErr != nil {
				return fmt.Errorf("detach surviving attachment %s from builder disk: %w", att.InstanceID, detachErr)
			}
			m.logger.Warn("detached surviving builder disk attachment", "instance", att.InstanceID, "volume", volID)
		}
	}
	return nil
}

// runIdleReaper periodically deletes builders idle past the configured TTL.
// Deletion is total and irreversible; the reaper only runs when IdleTTL > 0.
func (m *manager) runIdleReaper(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapIdle(ctx)
		}
	}
}

// reapIdle deletes ready builders whose last use (or creation) is older
// than the idle TTL. It marks a candidate deleting before clearing stale
// attachments, preventing a build from acquiring it during cleanup.
func (m *manager) reapIdle(ctx context.Context) {
	ids, err := listBuilderIDs(m.paths)
	if err != nil {
		m.logger.Error("idle reaper failed to list builders", "error", err)
		return
	}
	cutoff := time.Now().Add(-m.config.IdleTTL)

	for _, id := range ids {
		meta, shouldDelete, err := m.markIdleBuilderDeleting(id, cutoff)
		if err != nil {
			m.logger.Error("idle reaper failed to inspect builder", "id", id, "error", err)
			continue
		}
		if !shouldDelete {
			continue
		}

		if err := m.clearStaleAttachments(ctx, meta.diskVolumeID()); err != nil {
			m.logger.Error("idle reaper failed to clear stale attachments", "id", id, "error", err)
			continue
		}
		if err := m.DeleteBuilder(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			m.logger.Error("idle reaper failed to delete builder", "id", id, "error", err)
		}
	}
}

func (m *manager) markIdleBuilderDeleting(id string, cutoff time.Time) (*storedMetadata, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := loadMetadata(m.paths, id)
	if err != nil {
		return nil, false, err
	}
	if meta.Status == StatusDeleting {
		return meta, true, nil
	}
	if meta.Status != StatusReady {
		return meta, false, nil
	}
	if _, held := m.acquired[id]; held || m.buildsInFlightLocked(id) {
		return meta, false, nil
	}

	lastActivity := meta.CreatedAt
	if meta.LastUsedAt != nil {
		lastActivity = *meta.LastUsedAt
	}
	if lastActivity.After(cutoff) {
		return meta, false, nil
	}

	m.logger.Info("deleting idle builder", "id", id, "last_activity", lastActivity)
	meta.Status = StatusDeleting
	if err := saveMetadata(m.paths, meta); err != nil {
		return nil, false, err
	}
	return meta, true, nil
}
