//go:build linux

package instances

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	fsIOCFiemap      = 0xC020660B
	fiemapFlagSync   = 0x1
	fiemapMaxExtents = 64
)

type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

type fiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

type fiemapRequest struct {
	Header  fiemapHeader
	Extents [fiemapMaxExtents]fiemapExtent
}

func fileExtents(path string) ([]fiemapExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var req fiemapRequest
	req.Header.Length = ^uint64(0)
	req.Header.Flags = fiemapFlagSync
	req.Header.ExtentCount = fiemapMaxExtents

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(fsIOCFiemap), uintptr(unsafe.Pointer(&req))); errno != 0 {
		return nil, errno
	}
	return req.Extents[:req.Header.MappedExtents], nil
}

// assertCopyReflinked walks srcDir and verifies that at least one regular
// file shares a physical extent with its counterpart under dstDir. A
// successful FICLONE leaves the destination pointing at the source's
// extents, so FIEMAP will report identical fe_physical offsets. If every
// inspected pair has disjoint extents, the FICLONE fast path silently
// degraded to a full byte copy and we want to fail loudly. Requires a
// reflink-capable filesystem under the test's scratch directory (XFS with
// reflink=1 in CI).
func assertCopyReflinked(t *testing.T, srcDir, dstDir string) {
	t.Helper()

	type candidate struct {
		rel  string
		size int64
	}
	var candidates []candidate
	require.NoError(t, filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{rel: rel, size: info.Size()})
		return nil
	}))
	require.NotEmpty(t, candidates, "no non-empty regular files under %s", srcDir)

	var inspected, shared int
	for _, c := range candidates {
		dstPath := filepath.Join(dstDir, c.rel)
		if _, err := os.Stat(dstPath); err != nil {
			continue
		}
		srcExtents, err := fileExtents(filepath.Join(srcDir, c.rel))
		if err != nil {
			t.Logf("FIEMAP %s: %v", c.rel, err)
			continue
		}
		dstExtents, err := fileExtents(dstPath)
		if err != nil {
			t.Logf("FIEMAP %s: %v", dstPath, err)
			continue
		}
		inspected++
		if extentsShareAny(srcExtents, dstExtents) {
			shared++
		}
	}
	require.NotZero(t, inspected, "no files inspected for reflink sharing")
	require.NotZero(t, shared,
		"no files shared physical extents between %s and %s; FICLONE fast path produced full byte copies",
		srcDir, dstDir)
}

func extentsShareAny(a, b []fiemapExtent) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	seen := make(map[uint64]struct{}, len(a))
	for _, e := range a {
		seen[e.Physical] = struct{}{}
	}
	for _, e := range b {
		if _, ok := seen[e.Physical]; ok {
			return true
		}
	}
	return false
}
