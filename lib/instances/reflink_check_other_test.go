//go:build !linux

package instances

import "testing"

func assertCopyReflinked(t *testing.T, srcDir, dstDir string) {
	t.Helper()
	t.Logf("reflink assertion skipped on non-Linux (src=%s dst=%s)", srcDir, dstDir)
}
