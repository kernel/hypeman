package cloudhypervisor

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortTempDir returns a temp dir with a short prefix so unix socket
// paths stay under the platform sun_path limit (108 bytes on Linux,
// 104 on macOS). t.TempDir() under /var/folders on macOS overflows.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ch")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSerialReader_CopiesBytesToLog(t *testing.T) {
	tmp := shortTempDir(t)
	logPath := filepath.Join(tmp, "logs", "app.log")
	sockPath := serialSocketPath(logPath)

	// Reader starts first (mirrors production order: hypeman starts the
	// reader, then CH binds the socket during vm.create). The reader
	// dials with retry until the listener comes up.
	sr, err := startSerialReader(context.Background(), sockPath, logPath)
	require.NoError(t, err)
	t.Cleanup(sr.Close)

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	conn, err := ln.Accept()
	require.NoError(t, err)

	payload := []byte("hello from cloud hypervisor\n")
	_, err = conn.Write(payload)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(logPath)
		return err == nil && len(data) == len(payload)
	}, 2*time.Second, 10*time.Millisecond, "expected reader to flush bytes to log")

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, payload, data)
}

// TestSerialReader_NoSparseHoleAfterCopytruncate is the regression test
// for the bug that motivated this fix: copytruncate against a file whose
// writer holds a non-O_APPEND fd creates a sparse hole of NUL bytes from
// byte 0 to the writer's stale offset. Hypeman now owns the writer fd
// with O_APPEND, so post-truncate writes correctly resume at byte 0.
func TestSerialReader_NoSparseHoleAfterCopytruncate(t *testing.T) {
	tmp := shortTempDir(t)
	logPath := filepath.Join(tmp, "logs", "app.log")
	sockPath := serialSocketPath(logPath)

	sr, err := startSerialReader(context.Background(), sockPath, logPath)
	require.NoError(t, err)
	t.Cleanup(sr.Close)

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	conn, err := ln.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	pre := []byte("pre-rotate line\n")
	_, err = conn.Write(pre)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		st, err := os.Stat(logPath)
		return err == nil && st.Size() == int64(len(pre))
	}, 2*time.Second, 10*time.Millisecond)

	// Mimic rotateLogIfNeeded: copy then truncate the file out from under
	// the writer.
	require.NoError(t, copyFile(logPath, logPath+".1"))
	require.NoError(t, os.Truncate(logPath, 0))

	post := []byte("post-rotate line\n")
	_, err = conn.Write(post)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		st, err := os.Stat(logPath)
		return err == nil && st.Size() == int64(len(post))
	}, 2*time.Second, 10*time.Millisecond)

	st, err := os.Stat(logPath)
	require.NoError(t, err)
	sys := st.Sys().(*syscall.Stat_t)
	allocated := int64(sys.Blocks) * 512
	apparent := st.Size()
	assert.LessOrEqual(t, apparent, allocated, "post-truncate file is sparse: apparent=%d allocated=%d", apparent, allocated)

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, post, data, "post-truncate writes should land at byte 0, not at the writer's stale offset")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
