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
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"golang.org/x/sys/unix"
)

// ImportMacOSImage is an offline, administrator-only import of a STOPPED macvm
// bundle. It intentionally is not exposed as an HTTP host-filesystem read API.
// Imports preserve identity: instances from this image may run sequentially,
// not concurrently, until rekeying is validated.
func ImportMacOSImage(ctx context.Context, p *paths.Paths, name, source string) (*Image, error) {
	ref, err := ParseNormalizedRef(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	if ref.IsDigest() {
		return nil, fmt.Errorf("%w: import requires a tagged name", ErrInvalidName)
	}
	bundle, err := readMacOSMachineBundle(source, "disk.img", "aux.img", "config.json", false)
	if err != nil {
		return nil, err
	}
	mac := bundle.Platform
	for _, file := range []string{"disk.img", "aux.img"} {
		file = filepath.Join(source, file)
		info, e := os.Lstat(file)
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("bundle file must be regular: %s", file)
		}
		if e = exec.CommandContext(ctx, "lsof", "-t", file).Run(); e == nil {
			return nil, fmt.Errorf("bundle storage is open: stop its VM first")
		}
		if exit, ok := e.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("cannot verify storage is closed: %v", e)
		}
	}
	root := p.ImageRepositoryDir(ref.Repository())
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(root, ".macos-import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	for src, dst := range map[string]string{bundle.Disk: "rootfs.raw", bundle.Aux: "aux.img"} {
		if err = unix.Clonefile(src, filepath.Join(stage, dst), unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY); err != nil {
			return nil, fmt.Errorf("clone bundle: %w", err)
		}
		if err = os.Chmod(filepath.Join(stage, dst), 0600); err != nil {
			return nil, err
		}
	}
	// Hash cloned content, not the caller's mutable paths. Per-file hashes and
	// logical sizes make the digest unambiguous, including sparse zero ranges.
	h := sha256.New()
	canonical, _ := json.Marshal(mac)
	h.Write(canonical)
	var logicalSize int64
	for _, name := range []string{"rootfs.raw", "aux.img"} {
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
		if name == "rootfs.raw" {
			logicalSize = n
		}
	}
	digestHex := hex.EncodeToString(h.Sum(nil))
	digest := "sha256:" + digestHex
	meta := &imageMetadata{Name: ref.Repository() + "@" + digest, Digest: digest, Platform: "darwin/arm64", MacOS: mac, Status: StatusReady, SizeBytes: logicalSize, CreatedAt: time.Now().UTC()}
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
