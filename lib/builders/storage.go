package builders

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
)

// Filesystem structure:
// {dataDir}/builders/{builder-id}/
//   metadata.json   # Builder metadata

// storedMetadata represents builder metadata persisted to disk
type storedMetadata struct {
	ID         string     `json:"id"`
	Name       string     `json:"name,omitempty"`
	DiskSizeGb int        `json:"disk_size_gb"`
	Tags       tags.Tags  `json:"tags,omitempty"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// loadMetadata loads builder metadata from disk
func loadMetadata(p *paths.Paths, id string) (*storedMetadata, error) {
	if err := ValidateBuilderID(id); err != nil {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(p.BuilderMetadata(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta storedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// saveMetadata writes builder metadata to disk atomically
func saveMetadata(p *paths.Paths, meta *storedMetadata) error {
	if err := ValidateBuilderID(meta.ID); err != nil {
		return err
	}
	dir := p.BuilderDir(meta.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create builder directory: %w", err)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	tempPath := p.BuilderMetadata(meta.ID) + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("write temp metadata: %w", err)
	}
	if err := os.Rename(tempPath, p.BuilderMetadata(meta.ID)); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("rename metadata: %w", err)
	}

	return nil
}

// deleteBuilderData removes all builder data from disk
func deleteBuilderData(p *paths.Paths, id string) error {
	if err := ValidateBuilderID(id); err != nil {
		return ErrNotFound
	}
	if err := os.RemoveAll(p.BuilderDir(id)); err != nil {
		return fmt.Errorf("remove builder directory: %w", err)
	}
	return nil
}

// listBuilderIDs returns all builder IDs by scanning the builders directory
func listBuilderIDs(p *paths.Paths) ([]string, error) {
	entries, err := os.ReadDir(p.BuildersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read builders directory: %w", err)
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(p.BuilderMetadata(entry.Name())); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat builder metadata %s: %w", entry.Name(), err)
		}
		ids = append(ids, entry.Name())
	}
	return ids, nil
}

func (m *storedMetadata) diskVolumeID() string {
	return DiskVolumeID(m.ID)
}

// toBuilder converts stored metadata to the public Builder type
func (m *storedMetadata) toBuilder() *Builder {
	return &Builder{
		ID:           m.ID,
		Name:         m.Name,
		DiskSizeGb:   m.DiskSizeGb,
		Tags:         tags.Clone(m.Tags),
		Status:       m.Status,
		CreatedAt:    m.CreatedAt,
		LastUsedAt:   m.LastUsedAt,
		DiskVolumeID: DiskVolumeID(m.ID),
	}
}
