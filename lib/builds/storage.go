package builds

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/onkernel/hypeman/lib/paths"
)

// buildMetadata is the internal representation stored on disk
type buildMetadata struct {
	ID              string           `json:"id"`
	Status          string           `json:"status"`
	Runtime         string           `json:"runtime"`
	Request         *CreateBuildRequest `json:"request,omitempty"`
	ImageDigest     *string          `json:"image_digest,omitempty"`
	ImageRef        *string          `json:"image_ref,omitempty"`
	Error           *string          `json:"error,omitempty"`
	Provenance      *BuildProvenance `json:"provenance,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	StartedAt       *time.Time       `json:"started_at,omitempty"`
	CompletedAt     *time.Time       `json:"completed_at,omitempty"`
	DurationMS      *int64           `json:"duration_ms,omitempty"`
	BuilderInstance *string          `json:"builder_instance,omitempty"` // Instance ID of builder VM
}

// toBuild converts internal metadata to the public Build type
func (m *buildMetadata) toBuild() *Build {
	return &Build{
		ID:          m.ID,
		Status:      m.Status,
		Runtime:     m.Runtime,
		ImageDigest: m.ImageDigest,
		ImageRef:    m.ImageRef,
		Error:       m.Error,
		Provenance:  m.Provenance,
		CreatedAt:   m.CreatedAt,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
		DurationMS:  m.DurationMS,
	}
}

// writeMetadata writes build metadata to disk atomically
func writeMetadata(p *paths.Paths, meta *buildMetadata) error {
	dir := p.BuildDir(meta.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create build directory: %w", err)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	// Write atomically via temp file
	tempPath := p.BuildMetadata(meta.ID) + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("write temp metadata: %w", err)
	}

	finalPath := p.BuildMetadata(meta.ID)
	if err := os.Rename(tempPath, finalPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("rename metadata: %w", err)
	}

	return nil
}

// readMetadata reads build metadata from disk
func readMetadata(p *paths.Paths, id string) (*buildMetadata, error) {
	path := p.BuildMetadata(id)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta buildMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// listAllBuilds returns all builds sorted by creation time (newest first)
func listAllBuilds(p *paths.Paths) ([]*buildMetadata, error) {
	buildsDir := p.BuildsDir()

	entries, err := os.ReadDir(buildsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read builds directory: %w", err)
	}

	var metas []*buildMetadata
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		meta, err := readMetadata(p, entry.Name())
		if err != nil {
			continue // Skip invalid entries
		}
		metas = append(metas, meta)
	}

	// Sort by created_at descending (newest first)
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].CreatedAt.After(metas[j].CreatedAt)
	})

	return metas, nil
}

// listPendingBuilds returns builds that need to be recovered on startup
// Returns builds with status queued/building, sorted by created_at (oldest first for FIFO)
func listPendingBuilds(p *paths.Paths) ([]*buildMetadata, error) {
	all, err := listAllBuilds(p)
	if err != nil {
		return nil, err
	}

	var pending []*buildMetadata
	for _, meta := range all {
		switch meta.Status {
		case StatusQueued, StatusBuilding, StatusPushing:
			pending = append(pending, meta)
		}
	}

	// Sort by created_at ascending (oldest first for FIFO recovery)
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})

	return pending, nil
}

// deleteBuild removes a build's data from disk
func deleteBuild(p *paths.Paths, id string) error {
	dir := p.BuildDir(id)

	// Check if exists
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("stat build directory: %w", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove build directory: %w", err)
	}

	return nil
}

// ensureLogsDir ensures the logs directory exists for a build
func ensureLogsDir(p *paths.Paths, id string) error {
	logsDir := p.BuildLogs(id)
	return os.MkdirAll(logsDir, 0755)
}

// appendLog appends log data to the build log file
func appendLog(p *paths.Paths, id string, data []byte) error {
	if err := ensureLogsDir(p, id); err != nil {
		return err
	}

	logPath := p.BuildLog(id)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write log: %w", err)
	}

	return nil
}

// readLog reads the build log file
func readLog(p *paths.Paths, id string) ([]byte, error) {
	logPath := p.BuildLog(id)
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No logs yet
		}
		return nil, fmt.Errorf("read log: %w", err)
	}
	return data, nil
}

// writeBuildConfig writes the build config for the builder VM
func writeBuildConfig(p *paths.Paths, id string, config *BuildConfig) error {
	dir := p.BuildDir(id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create build directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal build config: %w", err)
	}

	configPath := p.BuildConfig(id)
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("write build config: %w", err)
	}

	return nil
}

