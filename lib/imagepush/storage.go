package imagepush

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/kernel/hypeman/lib/paths"
)

// pushMetadata is the internal representation stored on disk. Credentials are
// deliberately absent: borrowed credentials live only in memory for the
// duration of the push. HadCredentials records that the job used them so
// recovery can fail it instead of retrying without them.
type pushMetadata struct {
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	Image          string     `json:"image"`
	Digest         string     `json:"digest"`
	Target         string     `json:"target"`
	Insecure       bool       `json:"insecure"`
	HadCredentials bool       `json:"had_credentials,omitempty"`
	Error          *string    `json:"error,omitempty"`
	Layers         int        `json:"layers,omitempty"`
	Bytes          int64      `json:"bytes,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

func (m *pushMetadata) toPush() *Push {
	return &Push{
		ID:          m.ID,
		Image:       m.Image,
		Digest:      m.Digest,
		Target:      m.Target,
		Status:      m.Status,
		Error:       m.Error,
		Layers:      m.Layers,
		Bytes:       m.Bytes,
		CreatedAt:   m.CreatedAt,
		CompletedAt: m.CompletedAt,
	}
}

// writeMetadata writes push metadata to disk atomically.
func writeMetadata(p *paths.Paths, meta *pushMetadata) error {
	dir := p.PushDir(meta.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create push directory: %w", err)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	tempPath := p.PushMetadata(meta.ID) + ".tmp"
	file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create temp metadata: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tempPath)
		return fmt.Errorf("write temp metadata: %w", err)
	}
	// Sync before rename so a crash cannot leave an empty/partial final file
	// in place of a durably-written one.
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tempPath)
		return fmt.Errorf("sync temp metadata: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("close temp metadata: %w", err)
	}

	finalPath := p.PushMetadata(meta.ID)
	if err := os.Rename(tempPath, finalPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("rename metadata: %w", err)
	}

	return nil
}

func readMetadata(p *paths.Paths, id string) (*pushMetadata, error) {
	data, err := os.ReadFile(p.PushMetadata(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta pushMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// listAllPushes returns all pushes sorted by creation time (newest first).
func listAllPushes(p *paths.Paths) ([]*pushMetadata, error) {
	entries, err := os.ReadDir(p.PushesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pushes directory: %w", err)
	}

	metas := make([]*pushMetadata, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := readMetadata(p, entry.Name())
		if err != nil {
			// A missing metadata.json is not an anomaly: the dir belongs to a
			// CreatePush writing right now or to a crash orphan swept at the
			// next startup. Warning here would fire on every list for as long
			// as the dir lingers.
			if errors.Is(err, ErrNotFound) {
				continue
			}
			// Surface unreadable records instead of swallowing them: a corrupt
			// or half-written metadata.json would otherwise vanish from
			// listing and recovery while its push directory lingers on disk.
			fmt.Fprintf(os.Stderr, "Warning: skipping push %s with unreadable metadata: %v\n", entry.Name(), err)
			continue
		}
		metas = append(metas, meta)
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].CreatedAt.After(metas[j].CreatedAt)
	})

	return metas, nil
}

// removeOrphanedPushDirs deletes push directories that never received a
// metadata.json (a crash between MkdirAll and the metadata rename). They hold
// no record to recover and would otherwise linger on disk forever. Only safe
// at startup, before any CreatePush can be mid-write.
func removeOrphanedPushDirs(p *paths.Paths) {
	entries, err := os.ReadDir(p.PushesDir())
	if err != nil {
		return // nothing to sweep; listing errors surface in recovery proper
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(p.PushMetadata(entry.Name())); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "Warning: removing orphaned push directory %s (no metadata.json)\n", entry.Name())
			os.RemoveAll(p.PushDir(entry.Name()))
		}
	}
}

// listPendingPushes returns pushes that did not reach a terminal state,
// oldest first for FIFO recovery.
func listPendingPushes(p *paths.Paths) ([]*pushMetadata, error) {
	all, err := listAllPushes(p)
	if err != nil {
		return nil, err
	}

	pending := make([]*pushMetadata, 0)
	for _, meta := range all {
		switch meta.Status {
		case StatusQueued, StatusPushing:
			pending = append(pending, meta)
		}
	}

	sort.SliceStable(pending, func(i, j int) bool {
		// CreatedAt ties are broken by ID so recovery order — and with it the
		// "oldest record wins" dedup in recoverInterruptedPushes — is
		// deterministic across restarts.
		if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})

	return pending, nil
}
