package templates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// Registry persists and indexes templates. The default file-backed
// implementation stores one JSON file per template under
// paths.TemplatesDir(); higher-level callers (the instances manager) hold
// the registry and read it as a stable index.
//
// Registry is concurrency-safe; in-process locking keeps reads and writes
// consistent. Cross-process callers should not be writing to the same data
// dir simultaneously today; if/when that changes we'd add file locking.
type Registry interface {
	// Save inserts or replaces a template record.
	Save(ctx context.Context, t *Template) error

	// Get returns a template by its ID. ErrNotFound when missing.
	Get(ctx context.Context, id string) (*Template, error)

	// GetByName resolves a template by its unique name.
	GetByName(ctx context.Context, name string) (*Template, error)

	// List returns all templates, optionally filtered.
	List(ctx context.Context, filter *ListFilter) ([]*Template, error)

	// Delete removes a template. Returns ErrInUse when ForkCount > 0.
	Delete(ctx context.Context, id string) error

	// IncrementForkCount atomically bumps the fork refcount on a
	// template. Used at fork creation time.
	IncrementForkCount(ctx context.Context, id string) (*Template, error)

	// DecrementForkCount atomically drops the fork refcount on a
	// template (floor 0). Used when a fork is deleted. Touching
	// templates that were already deleted is a no-op.
	DecrementForkCount(ctx context.Context, id string) (*Template, error)

	// Reconcile walks the registry and rewrites ForkCount on every
	// template using observedForks: the count of live forks per
	// template id. Templates not present in observedForks fall to
	// zero. Used to heal drift after a crash, an out-of-band fork
	// delete, or any other path that bypassed Increment/Decrement.
	Reconcile(ctx context.Context, observedForks map[string]int) error
}

// ListFilter narrows the templates returned by Registry.List.
type ListFilter struct {
	// HypervisorType, when non-empty, restricts results to templates that
	// share the given hypervisor type. Forks must match the hypervisor
	// of their template.
	HypervisorType hypervisor.Type

	// ImageDigest, when non-empty, restricts results to templates whose
	// resolved image digest equals the given value. Useful when picking
	// a fan-out parent for a particular image revision.
	ImageDigest string
}

// FileRegistry is the default Registry implementation. It stores each
// template as a JSON file under TemplatesDir/<id>/template.json.
type FileRegistry struct {
	dir string
	mu  sync.Mutex
}

// NewFileRegistry returns a Registry that persists to dir. The directory is
// created on first write.
func NewFileRegistry(dir string) *FileRegistry {
	return &FileRegistry{dir: dir}
}

func (r *FileRegistry) path(id string) string {
	return filepath.Join(r.dir, id, "template.json")
}

func (r *FileRegistry) ensureDir(id string) error {
	return os.MkdirAll(filepath.Join(r.dir, id), 0o755)
}

func (r *FileRegistry) writeLocked(t *Template) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := r.ensureDir(t.ID); err != nil {
		return fmt.Errorf("create template dir: %w", err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal template: %w", err)
	}
	tmp := r.path(t.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write template tmp: %w", err)
	}
	if err := os.Rename(tmp, r.path(t.ID)); err != nil {
		return fmt.Errorf("rename template tmp: %w", err)
	}
	return nil
}

func (r *FileRegistry) readLocked(id string) (*Template, error) {
	data, err := os.ReadFile(r.path(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: id=%s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("read template: %w", err)
	}
	var t Template
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("unmarshal template %s: %w", id, err)
	}
	return &t, nil
}

func (r *FileRegistry) Save(ctx context.Context, t *Template) error {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeLocked(t)
}

func (r *FileRegistry) Get(ctx context.Context, id string) (*Template, error) {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readLocked(id)
}

func (r *FileRegistry) GetByName(ctx context.Context, name string) (*Template, error) {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.listLocked()
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if t.Name == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: name=%s", ErrNotFound, name)
}

func (r *FileRegistry) List(ctx context.Context, filter *ListFilter) ([]*Template, error) {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.listLocked()
	if err != nil {
		return nil, err
	}
	if filter == nil {
		return all, nil
	}
	out := make([]*Template, 0, len(all))
	for _, t := range all {
		if filter.HypervisorType != "" && t.HypervisorType != filter.HypervisorType {
			continue
		}
		if filter.ImageDigest != "" && t.ImageDigest != filter.ImageDigest {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (r *FileRegistry) listLocked() ([]*Template, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read templates dir: %w", err)
	}
	out := make([]*Template, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := r.readLocked(e.Name())
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r *FileRegistry) Delete(ctx context.Context, id string) error {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.readLocked(id)
	if err != nil {
		return err
	}
	if t.ForkCount > 0 {
		return fmt.Errorf("%w: %d live forks reference template %s", ErrInUse, t.ForkCount, id)
	}
	if err := os.RemoveAll(filepath.Join(r.dir, id)); err != nil {
		return fmt.Errorf("remove template dir: %w", err)
	}
	return nil
}

func (r *FileRegistry) IncrementForkCount(ctx context.Context, id string) (*Template, error) {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.readLocked(id)
	if err != nil {
		return nil, err
	}
	t.ForkCount++
	if err := r.writeLocked(t); err != nil {
		return nil, err
	}
	return t, nil
}

// Reconcile rewrites ForkCount on every persisted template using
// observedForks as the authority. Templates not present in observedForks
// are treated as having zero live forks. Errors on individual templates
// are returned as a wrapped multi-error so the caller can decide whether
// to treat partial reconciliation as fatal; reconciliation is best-effort
// and never deletes templates by itself.
func (r *FileRegistry) Reconcile(ctx context.Context, observedForks map[string]int) error {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.listLocked()
	if err != nil {
		return err
	}
	var firstErr error
	for _, t := range all {
		want := observedForks[t.ID]
		if t.ForkCount == want {
			continue
		}
		t.ForkCount = want
		if err := r.writeLocked(t); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("reconcile template %s: %w", t.ID, err)
		}
	}
	return firstErr
}

func (r *FileRegistry) DecrementForkCount(ctx context.Context, id string) (*Template, error) {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.readLocked(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if t.ForkCount > 0 {
		t.ForkCount--
	}
	if err := r.writeLocked(t); err != nil {
		return nil, err
	}
	return t, nil
}
