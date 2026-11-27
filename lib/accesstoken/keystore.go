package accesstoken

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// maxKeySetBytes bounds a stored key set, as the fetched one is bounded.
const maxKeySetBytes = 256 << 10

// FileKeyStore keeps each authorization server's last fetched public key set
// in one file of a directory, with the time it was fetched. A server that
// restarts continues from that copy.
//
// The copy is a convenience, not a record: a write that fails is logged and
// the fetched keys stay in memory. The older copy is then removed, so that a
// restart cannot bring back keys the server has withdrawn since.
type FileKeyStore struct {
	dir string
	log *slog.Logger
	// write replaces the file at a path. Tests make it fail.
	write func(path string, keySet []byte) error
}

// NewFileKeyStore stores key sets under dir, which is created when first needed.
func NewFileKeyStore(dir string, log *slog.Logger) *FileKeyStore {
	s := &FileKeyStore{dir: dir, log: log}
	s.write = s.replace
	return s
}

// path is the file of one key set. The id is the hex digest the verifier
// derives from an issuer and its key source; anything else names no file.
func (s *FileKeyStore) path(id string) (string, bool) {
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", false
	}
	return filepath.Join(s.dir, id+".json"), true
}

// Save replaces the stored key set.
func (s *FileKeyStore) Save(id, issuerURL string, keySet []byte) {
	path, ok := s.path(id)
	if !ok {
		return
	}
	err := s.write(path, keySet)
	if err == nil {
		return
	}
	// The copy that was not replaced may hold keys this fetch no longer
	// lists. Without a copy, a restart waits for a fetch.
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		s.log.Error("authorization server keys were not saved and the older copy could not be removed; a restart would start from the older keys", "issuer", issuerURL, "error", err, "remove_error", removeErr)
		return
	}
	s.log.Warn("authorization server keys were not saved; a restart will fetch them", "issuer", issuerURL, "error", err)
}

func (s *FileKeyStore) replace(path string, keySet []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.dir, ".keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(keySet); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// Load returns the stored key set, if there is a readable one.
func (s *FileKeyStore) Load(id string) ([]byte, bool) {
	path, ok := s.path(id)
	if !ok {
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxKeySetBytes {
		return nil, false
	}
	keySet, err := os.ReadFile(path)
	return keySet, err == nil
}
