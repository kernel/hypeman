package ocicache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"golang.org/x/sync/errgroup"
)

// indexMu serializes index.json read-modify-write cycles. Without it,
// concurrent appends lose descriptors or corrupt the file.
var indexMu sync.Mutex

// AppendImage writes img's blobs into the OCI layout at cacheDir, then appends
// a descriptor tagged with org.opencontainers.image.ref.name=tag to index.json.
//
// Every file is written to a temp file and renamed into place, so readers never
// see a partial blob or index and an interrupted write never replaces a good
// file. Blob writes run concurrently; the index update is serialized.
func AppendImage(cacheDir string, img v1.Image, tag string) error {
	if err := writeImage(cacheDir, img); err != nil {
		return err
	}
	retained, err := partial.Descriptor(img)
	if err != nil {
		return fmt.Errorf("describe image: %w", err)
	}
	// Copy before annotating: remote images hand back their own descriptor.
	desc := *retained
	desc.Annotations = make(map[string]string, len(retained.Annotations)+1)
	for k, v := range retained.Annotations {
		desc.Annotations[k] = v
	}
	desc.Annotations["org.opencontainers.image.ref.name"] = tag

	indexMu.Lock()
	defer indexMu.Unlock()
	return appendDescriptor(cacheDir, desc)
}

func writeImage(cacheDir string, img v1.Image) error {
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("list layers: %w", err)
	}
	var g errgroup.Group
	for _, layer := range layers {
		g.Go(func() error {
			digest, err := layer.Digest()
			if err != nil {
				return err
			}
			size, err := layer.Size()
			if err != nil {
				return err
			}
			rc, err := layer.Compressed()
			if err != nil {
				return err
			}
			return writeBlob(cacheDir, digest, size, rc)
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("write layers: %w", err)
	}

	cfgName, err := img.ConfigName()
	if err != nil {
		return err
	}
	cfg, err := img.RawConfigFile()
	if err != nil {
		return err
	}
	if err := writeBlob(cacheDir, cfgName, int64(len(cfg)), io.NopCloser(bytes.NewReader(cfg))); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return err
	}
	manifest, err := img.RawManifest()
	if err != nil {
		return err
	}
	if err := writeBlob(cacheDir, digest, int64(len(manifest)), io.NopCloser(bytes.NewReader(manifest))); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// WriteBlob stores data as the blob for hash, which the caller must have
// verified. Atomic and concurrency-safe like the blobs AppendImage writes.
func WriteBlob(cacheDir string, hash v1.Hash, data []byte) error {
	return writeBlob(cacheDir, hash, int64(len(data)), io.NopCloser(bytes.NewReader(data)))
}

// writeBlob skips the write when the blob already exists with the expected
// size, so a blob truncated by an interrupted writer is rewritten.
func writeBlob(cacheDir string, hash v1.Hash, size int64, rc io.ReadCloser) error {
	defer rc.Close()
	dir := filepath.Join(cacheDir, "blobs", hash.Algorithm)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(dir, hash.Hex)
	if info, err := os.Stat(final); err == nil && info.Size() == size {
		return nil
	}
	return writeFile(final, func(w io.Writer) error {
		n, err := io.Copy(w, rc)
		if err != nil {
			return err
		}
		if n != size {
			return fmt.Errorf("blob %s: wrote %d bytes, expected %d", hash, n, size)
		}
		return nil
	})
}

func appendDescriptor(cacheDir string, desc v1.Descriptor) error {
	layoutPath := filepath.Join(cacheDir, "oci-layout")
	if _, err := os.Stat(layoutPath); errors.Is(err, fs.ErrNotExist) {
		err = writeFile(layoutPath, func(w io.Writer) error {
			_, err := io.WriteString(w, `{"imageLayoutVersion":"1.0.0"}`)
			return err
		})
		if err != nil {
			return fmt.Errorf("create OCI layout: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("open OCI layout: %w", err)
	}

	indexPath := filepath.Join(cacheDir, "index.json")
	index, err := empty.Index.IndexManifest()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(indexPath)
	if err == nil {
		err = json.Unmarshal(data, index)
	} else if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("read OCI cache index: %w", err)
	}
	index.Manifests = append(index.Manifests, desc)
	data, err = json.MarshalIndent(index, "", "   ")
	if err != nil {
		return fmt.Errorf("encode OCI cache index: %w", err)
	}
	return writeFile(indexPath, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// writeFile writes to a temp file in the same directory, syncs it, and renames
// it over path. The temp name never matches a blob digest, so the cache GC
// ignores it.
func writeFile(path string, write func(io.Writer) error) error {
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
