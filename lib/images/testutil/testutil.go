// Package testutil provides helpers for seeding on-disk image state in tests.
package testutil

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

const seedImageContent = "rootfs!"

// Seed describes a ready image to seed directly to disk, bypassing pulls,
// builds, and conversion.
type Seed struct {
	Repository string
	DigestHex  string
	// Tag optionally creates a tag symlink pointing at the image.
	Tag string
	// Name overrides the metadata "name" field; it defaults to
	// repository:tag, or repository@sha256:digest when Tag is empty.
	Name string
	// Tags records resource tags in the metadata.
	Tags map[string]string
	// Content writes the image into the shared content layout instead of
	// the legacy per-repository digest layout.
	Content bool
}

type imageMetadata struct {
	Name      string            `json:"name"`
	Digest    string            `json:"digest"`
	Status    string            `json:"status"`
	SizeBytes int64             `json:"size_bytes"`
	Tags      map[string]string `json:"tags,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// SeedReadyImage writes a ready image to disk per s.
func SeedReadyImage(t testing.TB, p *paths.Paths, s Seed) {
	t.Helper()
	require.NoError(t, seedImage(p, s))
}

func seedImage(p *paths.Paths, s Seed) error {
	dir := p.ImageDigestDir(s.Repository, s.DigestHex)
	disk := p.ImageDigestPath(s.Repository, s.DigestHex)
	metadata := p.ImageMetadata(s.Repository, s.DigestHex)
	linkPath := p.ImageTagSymlink(s.Repository, s.Tag)
	target := s.DigestHex
	if s.Content {
		dir = p.ImageContentDir(s.DigestHex)
		disk = p.ImageContentPath(s.DigestHex)
		metadata = p.ImageContentMetadata(s.DigestHex)
		linkPath = p.ImageRepositoryTagSymlink(s.Repository, s.Tag)
		rel, err := filepath.Rel(filepath.Dir(linkPath), p.ImageContentDir(s.DigestHex))
		if err != nil {
			return fmt.Errorf("rel content symlink target: %w", err)
		}
		target = rel
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	if err := os.WriteFile(disk, []byte(seedImageContent), 0o644); err != nil {
		return fmt.Errorf("write disk image: %w", err)
	}

	name := s.Name
	if name == "" && s.Tag != "" {
		name = s.Repository + ":" + s.Tag
	} else if name == "" {
		name = s.Repository + "@sha256:" + s.DigestHex
	}
	data, err := json.Marshal(imageMetadata{
		Name:      name,
		Digest:    "sha256:" + s.DigestHex,
		Status:    images.StatusReady,
		SizeBytes: int64(len(seedImageContent)),
		Tags:      s.Tags,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(metadata, data, 0o644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	if s.Tag == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return fmt.Errorf("create tag dir: %w", err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		return fmt.Errorf("create tag symlink: %w", err)
	}
	return nil
}
