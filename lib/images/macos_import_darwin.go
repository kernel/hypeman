//go:build darwin && arm64

package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"golang.org/x/sys/unix"
)

// ImportMacOSImage is an offline, administrator-only import of a STOPPED macvm
// bundle. It intentionally is not exposed as an HTTP host-filesystem read API. The
// bundle's boot disk is disk.asif, cloned as is, or a raw disk.img, converted to
// ASIF. The image is a one-layer chain.
func ImportMacOSImage(ctx context.Context, p *paths.Paths, name, source string) (*Image, error) {
	ref, err := ParseNormalizedRef(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	if ref.IsDigest() {
		return nil, fmt.Errorf("%w: import requires a tagged name", ErrInvalidName)
	}
	diskName := "disk.asif"
	if _, err := os.Lstat(filepath.Join(source, diskName)); os.IsNotExist(err) {
		diskName = "disk.img"
	}
	bundle, err := readMacOSMachineBundle(source, diskName, "aux.img", "config.json")
	if err != nil {
		return nil, err
	}
	mac := bundle.Platform
	for _, file := range []string{diskName, "aux.img"} {
		file = filepath.Join(source, file)
		info, e := os.Lstat(file)
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("bundle file must be regular: %s", file)
		}
		if e = RequireClosedFile(ctx, file); e != nil {
			return nil, e
		}
	}
	root := p.ImageRepositoryDir(ref.Repository())
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// Stage outside the images tree, so the image index never lists a crashed import's
	// metadata. The data dir holds images, so the final rename stays on one filesystem.
	stage, err := os.MkdirTemp(p.DataDir(), ".macos-import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	clones := map[string]string{bundle.Aux: "aux.img"}
	if diskName == "disk.asif" {
		clones[bundle.Disks[0]] = machineDiskFile
	} else if err = ConvertRawToASIF(ctx, bundle.Disks[0], filepath.Join(stage, machineDiskFile)); err != nil {
		return nil, fmt.Errorf("convert boot disk to ASIF: %w", err)
	}
	for src, dst := range clones {
		if err = unix.Clonefile(src, filepath.Join(stage, dst), unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY); err != nil {
			return nil, fmt.Errorf("clone bundle: %w", err)
		}
	}
	for _, name := range []string{machineDiskFile, "aux.img"} {
		if err = os.Chmod(filepath.Join(stage, name), 0600); err != nil {
			return nil, err
		}
	}
	logicalSize, err := macOSChainSize([]string{filepath.Join(stage, machineDiskFile)})
	if err != nil {
		return nil, fmt.Errorf("imported boot disk: %w", err)
	}
	// Hash cloned content, not the caller's mutable paths. Per-file hashes and
	// logical sizes make the digest unambiguous, including sparse zero ranges.
	h := sha256.New()
	canonical, _ := json.Marshal(mac)
	h.Write(canonical)
	for _, name := range []string{machineDiskFile, "aux.img"} {
		f, e := os.Open(filepath.Join(stage, name))
		if e != nil {
			return nil, e
		}
		fh := sha256.New()
		n, e := io.Copy(fh, contextReader{ctx: ctx, reader: f})
		f.Close()
		if e != nil {
			return nil, e
		}
		fmt.Fprintf(h, "\n%s:%d:%x", name, n, fh.Sum(nil))
	}
	digestHex := hex.EncodeToString(h.Sum(nil))
	digest := "sha256:" + digestHex
	meta := &imageMetadata{Name: ref.Repository() + "@" + digest, Digest: digest, Platform: "darwin/arm64", MacOS: mac, MacOSDiskLayers: 1, Status: StatusReady, SizeBytes: logicalSize, CreatedAt: time.Now().UTC()}
	if err = writeMetadataFile(filepath.Join(stage, "metadata.json"), meta); err != nil {
		return nil, err
	}
	dst := p.ImageDigestDir(ref.Repository(), digestHex)
	if _, err = os.Stat(dst); os.IsNotExist(err) {
		if err = os.Rename(stage, dst); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		old, e := readMetadata(p, ref.Repository(), digestHex)
		if e != nil || old.Status != StatusReady || old.MacOS == nil {
			return nil, fmt.Errorf("existing import is incomplete")
		}
	}
	if err = createTagSymlink(p, ref.Repository(), ref.Tag(), digestHex); err != nil {
		return nil, err
	}
	return meta.toImageFor(ref.String()), nil
}
