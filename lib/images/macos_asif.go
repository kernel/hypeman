package images

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Code-Hex/vz/v4/diskimagekit"
	"github.com/kernel/hypeman/lib/paths"
)

// A macOS machine image's boot disk is an ASIF chain: disk.0.asif is the base and
// each later file is an overlay on the one before it. A pulled image has one file
// per OCI layer; an imported image has one.
func macOSDiskLayerFile(i int) string {
	return fmt.Sprintf("disk.%d.asif", i)
}

// MacOSDiskChain returns the boot disk chain of a ready macOS image, base first.
func MacOSDiskChain(p *paths.Paths, imageName, digest string) ([]string, error) {
	ref, err := ParseNormalizedRef(imageName)
	if err != nil {
		return nil, fmt.Errorf("parse image name: %w", err)
	}
	layout := resolveImageLayout(p, ref.Repository(), strings.TrimPrefix(digest, "sha256:"))
	meta, err := readMetadataAt(layout)
	if err != nil {
		return nil, err
	}
	if meta.MacOS == nil || meta.MacOSDiskLayers < 1 {
		return nil, fmt.Errorf("%w: image is not a macOS disk chain", ErrInvalidPlatform)
	}
	chain := make([]string, meta.MacOSDiskLayers)
	for i := range chain {
		chain[i] = filepath.Join(layout.dir, macOSDiskLayerFile(i))
	}
	return chain, nil
}

// OpenMacOSDiskChain opens an ASIF chain read-only, base first. Each layer must name
// the one below it as its parent.
func OpenMacOSDiskChain(chain []string) (*diskimagekit.DiskImage, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("empty macOS disk chain")
	}
	img, err := diskimagekit.Open(chain[0], diskimagekit.WithOpenMode(diskimagekit.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(chain[0]), err)
	}
	for _, layer := range chain[1:] {
		if err := img.Append(layer, diskimagekit.WithOpenMode(diskimagekit.ReadOnly)); err != nil {
			img.Close()
			return nil, fmt.Errorf("stack %s: %w", filepath.Base(layer), err)
		}
	}
	for i, info := range img.Layers() {
		if info.Format != diskimagekit.ASIF {
			img.Close()
			return nil, fmt.Errorf("macOS disk layer %d is not ASIF", i)
		}
	}
	return img, nil
}

// macOSChainSize validates a chain and returns its logical size in bytes.
func macOSChainSize(chain []string) (int64, error) {
	img, err := OpenMacOSDiskChain(chain)
	if err != nil {
		return 0, err
	}
	defer img.Close()
	return img.Size(), nil
}

// ConvertRawToASIF writes a raw disk into a new ASIF image at dst with the same
// logical size. All-zero chunks are skipped, so they stay unallocated.
func ConvertRawToASIF(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	const block = 512
	if info.Size() == 0 || info.Size()%block != 0 {
		return fmt.Errorf("raw disk size %d is not a positive multiple of %d", info.Size(), block)
	}
	out, err := diskimagekit.CreateASIF(dst, info.Size()/block)
	if err != nil {
		return err
	}
	const chunk = 1 << 20
	buf, zero := make([]byte, chunk), make([]byte, chunk)
	for offset := int64(0); offset < info.Size(); offset += chunk {
		if err = ctx.Err(); err != nil {
			break
		}
		n, readErr := io.ReadFull(io.NewSectionReader(in, offset, chunk), buf)
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			err = readErr
			break
		}
		if bytes.Equal(buf[:n], zero[:n]) {
			continue
		}
		if _, err = out.WriteAt(buf[:n], offset); err != nil {
			break
		}
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
