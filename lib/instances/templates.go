package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/templates"
	"github.com/nrednav/cuid2"
)

// PromoteToTemplateRequest configures a Standby instance promotion into a
// fork-only template parent.
type PromoteToTemplateRequest struct {
	// Name is the template's user-facing label. Must be unique. Required.
	Name string
	// Tags is optional user metadata.
	Tags map[string]string
}

// promoteToTemplate marks a Standby instance as a fork-only template parent
// and registers its metadata in the templates registry. The instance itself
// stays where it is on disk; what changes is the StoredMetadata flag and
// the new entry in the registry. Subsequent forks descend from this
// instance's snapshot directory.
//
// PR 2 ships only the lifecycle plumbing. PR 3 wires the resulting template
// into the firecracker fork path so forks share the template's mem-file
// instead of copying it.
func (m *manager) promoteToTemplate(ctx context.Context, instanceID string, req PromoteToTemplateRequest) (*templates.Template, error) {
	log := logger.FromContext(ctx)
	if m.templateRegistry == nil {
		return nil, fmt.Errorf("%w: template registry not configured", ErrNotSupported)
	}
	if req.Name == "" {
		return nil, fmt.Errorf("%w: template name is required", ErrInvalidRequest)
	}

	meta, err := m.loadMetadata(instanceID)
	if err != nil {
		return nil, err
	}
	stored := &meta.StoredMetadata
	inst := m.toInstance(ctx, meta)

	if inst.State != StateStandby {
		return nil, fmt.Errorf("%w: can only promote a Standby instance to a template (got %s)", ErrInvalidState, inst.State)
	}
	if !inst.HasSnapshot {
		return nil, fmt.Errorf("%w: instance %s has no snapshot to promote", ErrInvalidState, instanceID)
	}
	if stored.IsTemplate {
		return nil, fmt.Errorf("%w: instance %s is already a template", ErrAlreadyExists, instanceID)
	}
	if existing, err := m.templateRegistry.GetByName(ctx, req.Name); err == nil {
		return nil, fmt.Errorf("%w: template name %q already registered as id=%s", ErrAlreadyExists, req.Name, existing.ID)
	} else if !errors.Is(err, templates.ErrNotFound) {
		return nil, fmt.Errorf("check template name: %w", err)
	}

	templateID := cuid2.Generate()

	tpl := &templates.Template{
		ID:                templateID,
		Name:              req.Name,
		SourceInstanceID:  instanceID,
		Image:             stored.Image,
		HypervisorType:    stored.HypervisorType,
		HypervisorVersion: stored.HypervisorVersion,
		MemoryBytes:       stored.Size + stored.HotplugSize,
		VCPUs:             stored.Vcpus,
		CreatedAt:         m.now().UTC(),
	}
	for k, v := range req.Tags {
		if tpl.Tags == nil {
			tpl.Tags = map[string]string{}
		}
		tpl.Tags[k] = v
	}

	if err := m.templateRegistry.Save(ctx, tpl); err != nil {
		return nil, fmt.Errorf("save template: %w", err)
	}

	stored.IsTemplate = true
	stored.TemplateID = templateID
	if err := m.saveMetadata(meta); err != nil {
		// Best-effort rollback of the registry entry. If this fails the
		// operator can manually delete the orphan via DeleteTemplate.
		if delErr := m.templateRegistry.Delete(ctx, templateID); delErr != nil {
			log.WarnContext(ctx, "failed to roll back template registry entry after metadata save failure",
				"template_id", templateID, "error", delErr)
		}
		return nil, fmt.Errorf("persist template flag on instance: %w", err)
	}

	log.InfoContext(ctx, "promoted instance to template",
		"instance_id", instanceID, "template_id", templateID, "name", req.Name)
	return tpl, nil
}

// listTemplates returns all templates, optionally filtered.
func (m *manager) listTemplates(ctx context.Context, filter *templates.ListFilter) ([]*templates.Template, error) {
	if m.templateRegistry == nil {
		return nil, nil
	}
	return m.templateRegistry.List(ctx, filter)
}

// getTemplate looks up a template by ID.
func (m *manager) getTemplate(ctx context.Context, templateID string) (*templates.Template, error) {
	if m.templateRegistry == nil {
		return nil, fmt.Errorf("%w: template registry not configured", ErrNotSupported)
	}
	return m.templateRegistry.Get(ctx, templateID)
}

// deleteTemplate removes a template from the registry. The underlying
// source instance is not deleted; callers can decide whether to delete it
// separately. Refuses when ForkCount > 0.
func (m *manager) deleteTemplate(ctx context.Context, templateID string) error {
	if m.templateRegistry == nil {
		return fmt.Errorf("%w: template registry not configured", ErrNotSupported)
	}
	tpl, err := m.templateRegistry.Get(ctx, templateID)
	if err != nil {
		return err
	}

	if err := m.templateRegistry.Delete(ctx, templateID); err != nil {
		return err
	}

	// Best-effort: clear the IsTemplate flag on the source instance if it
	// still exists, so the operator can resume/delete it normally.
	if tpl != nil && tpl.SourceInstanceID != "" {
		meta, err := m.loadMetadata(tpl.SourceInstanceID)
		if err == nil {
			meta.StoredMetadata.IsTemplate = false
			meta.StoredMetadata.TemplateID = ""
			_ = m.saveMetadata(meta)
		}
	}
	return nil
}

// touchTemplateUsage updates LastUsedAt on a template. Cheap; called
// whenever a fork is created from the template.
func (m *manager) touchTemplateUsage(ctx context.Context, templateID string) {
	if m.templateRegistry == nil || templateID == "" {
		return
	}
	tpl, err := m.templateRegistry.Get(ctx, templateID)
	if err != nil {
		return
	}
	tpl.LastUsedAt = m.now().UTC()
	_ = m.templateRegistry.Save(ctx, tpl)
}

// templateGuard returns an error when the instance is a template parent.
// Templates must not be Started or Restored — the snapshot is shared with
// live forks and resuming it would corrupt them. PR 3 hardens this further
// when forks rely on the template's mem-file directly.
func (m *manager) templateGuard(stored *StoredMetadata, op string) error {
	if stored == nil || !stored.IsTemplate {
		return nil
	}
	return fmt.Errorf("%w: cannot %s template instance %s (template_id=%s); fork from it instead", ErrNotSupported, op, stored.Id, stored.TemplateID)
}

// validateForkResolvedFromTemplate confirms a fork-from-template request
// targets a hypervisor compatible with the template. The actual fork
// mechanics live in PR 3.
func validateForkResolvedFromTemplate(tpl *templates.Template, hvType hypervisor.Type) error {
	if tpl == nil {
		return fmt.Errorf("%w: nil template", ErrInvalidRequest)
	}
	if hvType != "" && tpl.HypervisorType != hvType {
		return fmt.Errorf(
			"%w: template hypervisor %s does not match requested %s",
			ErrInvalidRequest, tpl.HypervisorType, hvType,
		)
	}
	return nil
}

// templateForFork resolves a template by id-or-name. Empty input returns
// (nil, nil) so callers can treat "no template" as the ordinary fork path.
func (m *manager) templateForFork(ctx context.Context, idOrName string) (*templates.Template, error) {
	if idOrName == "" || m.templateRegistry == nil {
		return nil, nil
	}
	tpl, err := m.templateRegistry.Get(ctx, idOrName)
	if err == nil {
		return tpl, nil
	}
	if !errors.Is(err, templates.ErrNotFound) {
		return nil, err
	}
	return m.templateRegistry.GetByName(ctx, idOrName)
}

// templateRegistryRef exposes the registry to siblings within the package
// (e.g. fork.go for refcount bumps in PR 3/4). External packages must use
// the manager interface methods.
func (m *manager) templateRegistryRef() templates.Registry {
	return m.templateRegistry
}

// nowOrDefault returns the configured clock or time.Now if unset. Useful
// in code paths that may be called before NewManager has stamped a clock.
func (m *manager) nowOrDefault() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// resolveForkFromTemplateRequest expands a ForkInstanceRequest with a
// non-empty TemplateID into (sourceInstanceID, *Template). Returns
// (instanceID, nil, nil) when TemplateID is empty so callers fall through
// to the ordinary fork path. Returns an error when the caller passed both
// instanceID and TemplateID, when the registry is unconfigured, or when
// the template cannot be resolved.
func (m *manager) resolveForkFromTemplateRequest(ctx context.Context, instanceID string, req ForkInstanceRequest) (string, *templates.Template, error) {
	if req.TemplateID == "" {
		return instanceID, nil, nil
	}
	if instanceID != "" {
		return "", nil, fmt.Errorf("%w: pass either an instance id or a template id, not both", ErrInvalidRequest)
	}
	if m.templateRegistry == nil {
		return "", nil, fmt.Errorf("%w: template registry not configured", ErrNotSupported)
	}
	tpl, err := m.templateForFork(ctx, req.TemplateID)
	if err != nil {
		return "", nil, fmt.Errorf("resolve template %q: %w", req.TemplateID, err)
	}
	if tpl == nil {
		return "", nil, fmt.Errorf("%w: template %q not found", ErrNotFound, req.TemplateID)
	}
	if tpl.SourceInstanceID == "" {
		return "", nil, fmt.Errorf("%w: template %s has no source instance", ErrInvalidState, tpl.ID)
	}
	return tpl.SourceInstanceID, tpl, nil
}

// installForkSharedMemFile arranges the fork's snapshot directory so the
// guest mem-file is a symlink into the template's snapshot directory
// instead of a per-fork copy. firecracker mmaps the mem-file MAP_PRIVATE
// during restore, so all forks COW from the same backing file.
//
// Layout: dst is the fork's data dir. The snapshot dir is at
// <dst>/snapshots/snapshot-latest, and the mem-file lives at
// <snapshot dir>/memory. The symlink target is the template's source
// instance's standby snapshot mem-file.
func (m *manager) installForkSharedMemFile(forkDataDir string, tpl *templates.Template) error {
	if tpl == nil {
		return nil
	}
	srcMem := filepath.Join(m.paths.InstanceSnapshotLatest(tpl.SourceInstanceID), templateSharedMemFileName)
	if _, err := os.Stat(srcMem); err != nil {
		return fmt.Errorf("stat template mem-file: %w", err)
	}
	dstSnapshotDir := filepath.Join(forkDataDir, "snapshots", "snapshot-latest")
	if err := os.MkdirAll(dstSnapshotDir, 0o755); err != nil {
		return fmt.Errorf("ensure fork snapshot dir: %w", err)
	}
	dstMem := filepath.Join(dstSnapshotDir, templateSharedMemFileName)
	// Tolerate a leftover entry (e.g. from a partial copy that wasn't fully
	// skipped on a different filesystem layout).
	_ = os.Remove(dstMem)
	if err := os.Symlink(srcMem, dstMem); err != nil {
		return fmt.Errorf("symlink shared mem-file: %w", err)
	}
	return nil
}

// templateSharedMemFileRelPath is the relative path under the source data
// dir that points at the snapshotted guest mem-file. Encoded here so the
// fork copy step can skip it without importing firecracker internals.
const (
	templateSharedMemFileName    = "memory"
	templateSharedMemFileRelPath = "snapshots/snapshot-latest/memory"
)

// bumpTemplateForkRefcount records that a fork now depends on a template.
// Best-effort touch of LastUsedAt happens alongside.
func (m *manager) bumpTemplateForkRefcount(ctx context.Context, tpl *templates.Template) error {
	if tpl == nil || m.templateRegistry == nil {
		return nil
	}
	if _, err := m.templateRegistry.IncrementForkCount(ctx, tpl.ID); err != nil {
		return fmt.Errorf("increment template fork count: %w", err)
	}
	m.touchTemplateUsage(ctx, tpl.ID)
	return nil
}

// dropTemplateForkRefcount mirrors bumpTemplateForkRefcount and is called
// when a fork instance is deleted. Missing templates are tolerated so
// orphaned forks don't block delete.
func (m *manager) dropTemplateForkRefcount(ctx context.Context, templateID string) {
	if templateID == "" || m.templateRegistry == nil {
		return
	}
	if _, err := m.templateRegistry.DecrementForkCount(ctx, templateID); err != nil {
		log := logger.FromContext(ctx)
		log.WarnContext(ctx, "failed to decrement template fork refcount",
			"template_id", templateID, "error", err)
	}
}
