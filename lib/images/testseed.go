package images

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/paths"
)

const seedImageContent = "rootfs!"

// TestSeed describes a ready image to write directly to disk, bypassing pulls,
// builds, and conversion.
type TestSeed struct {
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

// seedImagePaths holds the on-disk locations for one seeded image, in either
// the shared content layout or the legacy per-repository digest layout.
type seedImagePaths struct {
	dir      string
	disk     string
	metadata string
}

func seedLayout(p *paths.Paths, s TestSeed) seedImagePaths {
	if s.Content {
		return seedImagePaths{
			dir:      p.ImageContentDir(s.DigestHex),
			disk:     p.ImageContentPath(s.DigestHex),
			metadata: p.ImageContentMetadata(s.DigestHex),
		}
	}
	return seedImagePaths{
		dir:      p.ImageDigestDir(s.Repository, s.DigestHex),
		disk:     p.ImageDigestPath(s.Repository, s.DigestHex),
		metadata: p.ImageMetadata(s.Repository, s.DigestHex),
	}
}

// SeedTestImage writes a ready image to disk per s. It is exported for
// lib/images/testutil so test seeds marshal imageMetadata directly instead of
// re-declaring the schema; production code does not use it.
func SeedTestImage(p *paths.Paths, s TestSeed) error {
	layout := seedLayout(p, s)
	if err := os.MkdirAll(layout.dir, 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	if err := os.WriteFile(layout.disk, []byte(seedImageContent), 0o644); err != nil {
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
		Status:    StatusReady,
		SizeBytes: int64(len(seedImageContent)),
		Tags:      s.Tags,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(layout.metadata, data, 0o644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	return seedTagSymlink(p, s)
}

// seedTagSymlink creates the tag symlink when s.Tag is set, pointing at the
// shared content directory or the bare digest per s.Content.
func seedTagSymlink(p *paths.Paths, s TestSeed) error {
	if s.Tag == "" {
		return nil
	}
	linkPath := p.ImageTagSymlink(s.Repository, s.Tag)
	target := s.DigestHex
	if s.Content {
		linkPath = p.ImageRepositoryTagSymlink(s.Repository, s.Tag)
		rel, err := filepath.Rel(filepath.Dir(linkPath), p.ImageContentDir(s.DigestHex))
		if err != nil {
			return fmt.Errorf("rel content symlink target: %w", err)
		}
		target = rel
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return fmt.Errorf("create tag dir: %w", err)
	}
	return os.Symlink(target, linkPath)
}
