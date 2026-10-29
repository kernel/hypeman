package images

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
)

// machineUnpackChunk is the granularity of hole detection when writing machine files.
const machineUnpackChunk = 64 << 10

var machineZeroChunk = make([]byte, machineUnpackChunk)

// maxMachineUnpackedBytes caps the decompressed bytes of all of a machine image's
// layers, so a small layer cannot expand without bound. Zero chunks become holes,
// so the cap bounds decompression and hashing work as much as disk use. Tests
// lower it.
var maxMachineUnpackedBytes int64 = 1 << 40

// unpackMachineLayers extracts a macOS machine image's layers into targetDir. Unlike
// the rootfs unpack, it writes files sparsely: zero chunks become holes, so a
// mostly empty boot disk occupies only its data. Machine bundles hold only regular
// files and directories; any other entry is rejected. Each layer is checked
// against its config diff ID.
func (c *ociClient) unpackMachineLayers(ctx context.Context, layoutTag, targetDir string) error {
	path, err := layout.FromPath(c.cacheDir)
	if err != nil {
		return fmt.Errorf("open oci layout: %w", err)
	}
	img, err := imageByAnnotation(path, layoutTag)
	if err != nil {
		return fmt.Errorf("find image by tag %s: %w", layoutTag, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	configFile, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("get config file: %w", err)
	}
	if err := validateConfigFileForUnpack(layoutTag, manifest, configFile); err != nil {
		return err
	}
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("get layers: %w", err)
	}
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	remaining := maxMachineUnpackedBytes
	for i, l := range layers {
		if err := unpackMachineLayer(ctx, l, configFile.RootFS.DiffIDs[i], targetDir, &remaining); err != nil {
			return fmt.Errorf("unpack machine layer %d: %w", i, err)
		}
	}
	return nil
}

// unpackMachineLayer extracts one layer, spending its decompressed bytes from the
// bundle's remaining budget. A delta layer patches the boot disk unpacked beneath it
// instead of writing files.
func unpackMachineLayer(ctx context.Context, l gcr.Layer, diffID gcr.Hash, targetDir string, remaining *int64) error {
	rc, err := l.Uncompressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	hash := sha256.New()
	// One byte past the budget distinguishes an exact fit from an overrun.
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: rc}, N: *remaining + 1}
	defer func() { *remaining -= *remaining + 1 - limited.N }()
	stream := io.TeeReader(limited, hash)
	// A read cut short by the limit surfaces as a truncated archive; report the limit.
	overLimit := func(err error) error {
		if limited.N == 0 {
			return fmt.Errorf("machine image exceeds the %d-byte unpacked size limit", maxMachineUnpackedBytes)
		}
		return err
	}
	archive := tar.NewReader(stream)
	mediaType, err := l.MediaType()
	if err != nil {
		return err
	}
	delta := mediaType == MacOSDeltaMediaType
	applier := &macOSDeltaApplier{targetDir: targetDir}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return overLimit(err)
		}
		name := filepath.Clean(header.Name)
		if !filepath.IsLocal(name) || strings.HasPrefix(filepath.Base(name), ".wh.") {
			return fmt.Errorf("machine layer entry %q is not a plain local path", header.Name)
		}
		if delta {
			if header.Typeflag != tar.TypeReg || isGNUSparse(header) {
				return fmt.Errorf("delta layer entry %q must be a regular file", header.Name)
			}
			if err := applier.entry(name, header.Size, archive); err != nil {
				return overLimit(err)
			}
			continue
		}
		target := filepath.Join(targetDir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			// The tar reader expands GNU sparse entries itself, writing holes it never
			// reads, so their size would escape the byte budget. Bundles never need them.
			if isGNUSparse(header) {
				return fmt.Errorf("machine layer entry %q is a GNU sparse tar entry, which machine images do not support", header.Name)
			}
			if header.Size >= limited.N {
				return fmt.Errorf("machine layer entry %q declares %d bytes, over the %d-byte unpacked size limit", header.Name, header.Size, maxMachineUnpackedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			if err := writeSparseFile(target, archive, header.Size); err != nil {
				return overLimit(fmt.Errorf("write %s: %w", name, err))
			}
		default:
			return fmt.Errorf("machine layer entry %q has unsupported type %q", header.Name, header.Typeflag)
		}
	}
	if delta {
		if err := applier.complete(); err != nil {
			return overLimit(err)
		}
	}
	// Hash the trailing tar padding too, so the digest covers the whole stream.
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return overLimit(err)
	}
	if err := overLimit(nil); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != diffID.Hex {
		return fmt.Errorf("layer content sha256:%s does not match diff ID %s", got, diffID)
	}
	return nil
}

// isGNUSparse reports whether a tar entry carries PAX GNU sparse records. The older
// GNU sparse type keeps its own typeflag and is rejected as unsupported.
func isGNUSparse(header *tar.Header) bool {
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// writeSparseFile writes size bytes from r to path. The file is sized first and
// only nonzero chunks are written, so zero chunks stay holes.
func writeSparseFile(path string, r io.Reader, size int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = file.Truncate(size)
	buf := make([]byte, machineUnpackChunk)
	for offset := int64(0); err == nil && offset < size; {
		chunk := buf[:min(int64(len(buf)), size-offset)]
		if _, err = io.ReadFull(r, chunk); err != nil {
			break
		}
		if !bytes.Equal(chunk, machineZeroChunk[:len(chunk)]) {
			_, err = file.WriteAt(chunk, offset)
		}
		offset += int64(len(chunk))
	}
	return errors.Join(err, file.Close())
}
