package instances

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/templates"
	"github.com/kernel/hypeman/lib/uffd"
)

// uffdTracker owns one uffd.Server per template mem-file and tracks which
// forks are currently restored against each one. The first
// acquireUffdForFork for a template lazily starts the server; the last
// releaseUffdForFork tears it down. This keeps the server out of the
// critical path for non-template forks (the symlink-only path from
// PR 3) and avoids leaking page-server goroutines once a template
// becomes idle.
//
// Lifecycle assumption: this PR scopes uffd to the *active* fork-create
// path. After a hypeman restart, any firecracker process previously
// backed by a uffd server has no one to handle its faults until those
// forks are themselves restarted; that gap is documented in
// recoverTemplateForkRefcounts and is the next follow-up.
type uffdTracker struct {
	mu      sync.Mutex
	entries map[string]*uffdEntry
}

type uffdEntry struct {
	server *uffd.Server
	forks  map[string]struct{}
}

func newUffdTracker() *uffdTracker {
	return &uffdTracker{entries: map[string]*uffdEntry{}}
}

// acquireUffdForFork ensures a uffd.Server is running for the template,
// registers forkID with it, and returns the per-fork UDS path. Callers
// must call releaseUffdForFork(templateID, forkID) once the fork no
// longer exists, even if firecracker never connected.
func (t *uffdTracker) acquireUffdForFork(ctx context.Context, tpl *templates.Template, memFilePath, socketDir, forkID string) (string, error) {
	if tpl == nil {
		return "", errors.New("uffd: template is required")
	}
	if forkID == "" {
		return "", errors.New("uffd: fork id is required")
	}
	t.mu.Lock()
	entry, ok := t.entries[tpl.ID]
	if !ok {
		srv, err := uffd.NewServer(uffd.Config{
			MemFilePath: memFilePath,
			SocketDir:   socketDir,
		})
		if err != nil {
			t.mu.Unlock()
			return "", fmt.Errorf("uffd: start server for template %s: %w", tpl.ID, err)
		}
		entry = &uffdEntry{server: srv, forks: map[string]struct{}{}}
		t.entries[tpl.ID] = entry
	}
	t.mu.Unlock()

	socketPath, err := entry.server.RegisterFork(ctx, forkID)
	if err != nil {
		t.maybeCloseEmpty(tpl.ID)
		return "", fmt.Errorf("uffd: register fork %s with template %s: %w", forkID, tpl.ID, err)
	}

	t.mu.Lock()
	entry.forks[forkID] = struct{}{}
	t.mu.Unlock()
	return socketPath, nil
}

// releaseUffdForFork unregisters the fork from its template's server
// and shuts the server down once no forks remain. It is safe to call
// for templates that never had a server (returns nil).
func (t *uffdTracker) releaseUffdForFork(templateID, forkID string) error {
	if templateID == "" || forkID == "" {
		return nil
	}
	t.mu.Lock()
	entry, ok := t.entries[templateID]
	if !ok {
		t.mu.Unlock()
		return nil
	}
	if _, present := entry.forks[forkID]; !present {
		t.mu.Unlock()
		return nil
	}
	delete(entry.forks, forkID)
	srv := entry.server
	empty := len(entry.forks) == 0
	if empty {
		delete(t.entries, templateID)
	}
	t.mu.Unlock()

	var firstErr error
	if err := srv.UnregisterFork(forkID); err != nil {
		firstErr = err
	}
	if empty {
		if err := srv.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// maybeCloseEmpty drops a template's entry when no forks ever attached
// to a freshly-created server (acquire-then-RegisterFork failure).
func (t *uffdTracker) maybeCloseEmpty(templateID string) {
	t.mu.Lock()
	entry, ok := t.entries[templateID]
	if !ok || len(entry.forks) > 0 {
		t.mu.Unlock()
		return
	}
	delete(t.entries, templateID)
	srv := entry.server
	t.mu.Unlock()
	_ = srv.Close()
}

// closeAll tears down every server. Called by the manager during
// shutdown so the uffd goroutines and mem-file fds don't leak.
func (t *uffdTracker) closeAll() error {
	t.mu.Lock()
	entries := t.entries
	t.entries = map[string]*uffdEntry{}
	t.mu.Unlock()

	var firstErr error
	for _, entry := range entries {
		for forkID := range entry.forks {
			_ = entry.server.UnregisterFork(forkID)
		}
		if err := entry.server.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// hasFork is a test-only helper that reports whether forkID is currently
// tracked under templateID.
func (t *uffdTracker) hasFork(templateID, forkID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[templateID]
	if !ok {
		return false
	}
	_, present := entry.forks[forkID]
	return present
}

// uffdSupportedForFork reports whether the manager will try to serve a
// fork's mem-file via uffd. Only firecracker is wired to consume the
// backend; other hypervisors fall back to the symlink-only path.
func uffdSupportedForFork(hvType hypervisor.Type) bool {
	return hvType == hypervisor.TypeFirecracker
}

// acquireForkUffdIfApplicable returns a per-fork uffd UDS path when the
// fork should be backed by a userfaultfd page server, or "" when it
// should fall back to the symlink-only path. Failure to start the
// server is bubbled up as an error so the caller can abort the fork.
func (m *manager) acquireForkUffdIfApplicable(ctx context.Context, tpl *templates.Template, forkID string, hvType hypervisor.Type) (string, error) {
	if tpl == nil || !uffdSupportedForFork(hvType) || m.uffd == nil {
		return "", nil
	}
	memFilePath := filepath.Join(m.paths.InstanceSnapshotLatest(tpl.SourceInstanceID), templateSharedMemFileName)
	socketDir := m.paths.TemplateUffdDir(tpl.ID)
	return m.uffd.acquireUffdForFork(ctx, tpl, memFilePath, socketDir, forkID)
}
