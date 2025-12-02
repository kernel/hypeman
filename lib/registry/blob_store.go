package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/onkernel/hypeman/lib/paths"
)

// notFoundError is a custom error that matches go-containerregistry's errNotFound sentinel.
// go-containerregistry uses errors.Is(err, errNotFound) where errNotFound = errors.New("not found").
// By implementing Is(), we ensure our error matches their sentinel via errors.Is().
type notFoundError struct{}

func (e notFoundError) Error() string { return "not found" }

func (e notFoundError) Is(target error) bool {
	return target.Error() == "not found"
}

// ErrNotFound is returned when a blob is not found.
var ErrNotFound = notFoundError{}

// BlobStore implements blob storage on the filesystem.
type BlobStore struct {
	paths *paths.Paths
}

// NewBlobStore creates a new filesystem-backed blob store.
func NewBlobStore(p *paths.Paths) (*BlobStore, error) {
	blobDir := p.OCICacheBlobDir()
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return nil, fmt.Errorf("create blob directory: %w", err)
	}
	return &BlobStore{paths: p}, nil
}

func (s *BlobStore) blobPath(digest string) string {
	digestHex := strings.TrimPrefix(digest, "sha256:")
	return s.paths.OCICacheBlob(digestHex)
}

func (s *BlobStore) Stat(_ context.Context, repo string, h v1.Hash) (int64, error) {
	path := s.blobPath(h.String())
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return info.Size(), nil
}

func (s *BlobStore) Get(_ context.Context, repo string, h v1.Hash) (io.ReadCloser, error) {
	path := s.blobPath(h.String())
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

func (s *BlobStore) Put(_ context.Context, repo string, h v1.Hash, r io.ReadCloser) error {
	defer r.Close()
	path := s.blobPath(h.String())
	if _, err := os.Stat(path); err == nil {
		io.Copy(io.Discard, r)
		return nil
	}
	tempPath := path + ".tmp"
	f, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("create temp blob file: %w", err)
	}
	defer func() {
		f.Close()
		os.Remove(tempPath)
	}()
	hasher := sha256.New()
	tee := io.TeeReader(r, hasher)
	if _, err := io.Copy(f, tee); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close blob file: %w", err)
	}
	actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if actualDigest != h.String() {
		return fmt.Errorf("digest mismatch: expected %s, got %s", h.String(), actualDigest)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename blob: %w", err)
	}
	return nil
}

func (s *BlobStore) Delete(_ context.Context, repo string, h v1.Hash) error {
	path := s.blobPath(h.String())
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
