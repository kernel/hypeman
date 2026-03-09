package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman/lib/paths"
)

var (
	ErrNotFound   = errors.New("snapshot not found")
	ErrNameExists = errors.New("snapshot name already exists")
)

// Record is the persisted representation of a snapshot plus source metadata.
type Record struct {
	Snapshot       Snapshot        `json:"snapshot"`
	StoredMetadata json.RawMessage `json:"stored_metadata"`
}

// Store handles snapshot metadata persistence under paths.SnapshotStoreDir().
type Store struct {
	paths *paths.Paths
}

func NewStore(p *paths.Paths) *Store {
	return &Store{paths: p}
}

func (s *Store) List(filter *ListSnapshotsFilter) ([]Snapshot, error) {
	records, err := s.ListRecords()
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(records))
	for _, rec := range records {
		snap := rec.Snapshot
		if filter == nil || filter.Matches(&snap) {
			out = append(out, snap)
		}
	}
	return out, nil
}

func (s *Store) Get(snapshotID string) (*Snapshot, error) {
	record, err := s.LoadRecord(snapshotID)
	if err != nil {
		return nil, err
	}
	out := record.Snapshot
	return &out, nil
}

func (s *Store) SaveRecord(record *Record) error {
	if record == nil {
		return fmt.Errorf("nil snapshot record")
	}
	dir := s.paths.SnapshotDir(record.Snapshot.Id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	if err := os.WriteFile(s.paths.SnapshotMetadata(record.Snapshot.Id), content, 0644); err != nil {
		return fmt.Errorf("write snapshot metadata: %w", err)
	}
	return nil
}

func (s *Store) LoadRecord(snapshotID string) (*Record, error) {
	content, err := os.ReadFile(s.paths.SnapshotMetadata(snapshotID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var record Record
	if err := json.Unmarshal(content, &record); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot metadata: %w", err)
	}
	if record.Snapshot.Id == "" {
		record.Snapshot.Id = snapshotID
	}
	return &record, nil
}

func (s *Store) ListRecords() ([]Record, error) {
	if err := os.MkdirAll(s.paths.SnapshotStoreDir(), 0755); err != nil {
		return nil, fmt.Errorf("create snapshot store directory: %w", err)
	}
	entries, err := os.ReadDir(s.paths.SnapshotStoreDir())
	if err != nil {
		return nil, fmt.Errorf("read snapshot store directory: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, err := s.LoadRecord(entry.Name())
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		records = append(records, *record)
	}
	return records, nil
}

func (s *Store) Delete(snapshotID string) error {
	path := s.paths.SnapshotDir(snapshotID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("stat snapshot directory: %w", err)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("delete snapshot: %w", err)
	}
	return nil
}

func (s *Store) EnsureNameAvailable(sourceInstanceID, snapshotName string) error {
	if snapshotName == "" {
		return nil
	}
	records, err := s.ListRecords()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Snapshot.SourceInstanceID == sourceInstanceID && record.Snapshot.Name == snapshotName {
			return fmt.Errorf("%w: snapshot name %q already exists for source instance %s", ErrNameExists, snapshotName, sourceInstanceID)
		}
	}
	return nil
}

func DirectoryFileSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk snapshot payload: %w", err)
	}
	return total, nil
}
