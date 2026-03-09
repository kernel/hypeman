package snapshottransfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/kernel/hypeman/lib/paths"
)

type Store struct {
	paths *paths.Paths
}

func NewStore(p *paths.Paths) *Store {
	return &Store{paths: p}
}

func (s *Store) SaveTransfer(rec *TransferRecord) error {
	if rec == nil {
		return fmt.Errorf("nil transfer record")
	}
	dir := s.paths.SnapshotTransferDir(rec.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create transfer dir: %w", err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal transfer: %w", err)
	}
	if err := os.WriteFile(s.paths.SnapshotTransferMetadata(rec.ID), b, 0600); err != nil {
		return fmt.Errorf("write transfer metadata: %w", err)
	}
	return nil
}

func (s *Store) LoadTransfer(id string) (*TransferRecord, error) {
	b, err := os.ReadFile(s.paths.SnapshotTransferMetadata(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrTransferNotFound
		}
		return nil, fmt.Errorf("read transfer metadata: %w", err)
	}
	var rec TransferRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal transfer metadata: %w", err)
	}
	return &rec, nil
}

func (s *Store) ListTransfers() ([]TransferRecord, error) {
	if err := os.MkdirAll(s.paths.SnapshotTransfersDir(), 0700); err != nil {
		return nil, fmt.Errorf("create transfers dir: %w", err)
	}
	entries, err := os.ReadDir(s.paths.SnapshotTransfersDir())
	if err != nil {
		return nil, fmt.Errorf("read transfers dir: %w", err)
	}
	out := make([]TransferRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rec, err := s.LoadTransfer(entry.Name())
		if err != nil {
			if errors.Is(err, ErrTransferNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) SaveTransferManifest(transferID string, manifest Manifest) error {
	dir := s.paths.SnapshotTransferDir(transferID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create transfer dir: %w", err)
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(s.paths.SnapshotTransferManifest(transferID), b, 0600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

func (s *Store) LoadTransferManifest(transferID string) (*Manifest, error) {
	b, err := os.ReadFile(s.paths.SnapshotTransferManifest(transferID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrTransferNotFound
		}
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}
	return &m, nil
}

func (s *Store) SaveSession(rec *ImportSessionRecord) error {
	if rec == nil {
		return fmt.Errorf("nil session record")
	}
	dir := s.paths.SnapshotImportSessionDir(rec.ID)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	if err := os.WriteFile(s.paths.SnapshotImportSessionMetadata(rec.ID), b, 0600); err != nil {
		return fmt.Errorf("write session metadata: %w", err)
	}
	return nil
}

func (s *Store) LoadSession(id string) (*ImportSessionRecord, error) {
	b, err := os.ReadFile(s.paths.SnapshotImportSessionMetadata(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("read session metadata: %w", err)
	}
	var rec ImportSessionRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal session metadata: %w", err)
	}
	if rec.CommittedChunks == nil {
		rec.CommittedChunks = map[int]bool{}
	}
	return &rec, nil
}

func (s *Store) ListSessions() ([]ImportSessionRecord, error) {
	if err := os.MkdirAll(s.paths.SnapshotImportSessionsDir(), 0700); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}
	entries, err := os.ReadDir(s.paths.SnapshotImportSessionsDir())
	if err != nil {
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}
	out := make([]ImportSessionRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rec, err := s.LoadSession(entry.Name())
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}
