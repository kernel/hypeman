//go:build !linux

package instances

import "testing"

func assertCopyReflinked(t *testing.T, srcDir, dstDir string) {
	t.Helper()
	t.Logf("reflink assertion skipped on non-Linux (src=%s dst=%s)", srcDir, dstDir)
}

func assertCopyReflinkedGated(t *testing.T, probeDir, srcDir, dstDir string) {
	t.Helper()
	_ = probeDir
	assertCopyReflinked(t, srcDir, dstDir)
}
