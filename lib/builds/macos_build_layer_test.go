package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMachineLayerRegeneratesHashedBytesWithoutSpooling(t *testing.T) {
	parent := t.TempDir()
	disk := filepath.Join(parent, "disk.asif")
	data := make([]byte, 3<<20) // mostly zeros, like a sparse machine disk
	copy(data[1<<20:], "data")
	require.NoError(t, os.WriteFile(disk, data, 0600))

	blob, diffID, err := machineLayer(context.Background(), []machineTarEntry{{name: "disk.asif", path: disk}, {name: "config.json", data: []byte("{}")}})
	require.NoError(t, err)
	require.NotEmpty(t, diffID.Hex)
	for range 2 {
		body, err := blob.open()
		require.NoError(t, err)
		sum := sha256.New()
		n, err := io.Copy(sum, body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		require.Equal(t, blob.descriptor.Size, n)
		require.Equal(t, blob.descriptor.Digest.Hex, hex.EncodeToString(sum.Sum(nil)))
	}
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing is spooled beside the disk")
}

func TestMachineLayerAbandonedUploadStopsGenerator(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.asif")
	require.NoError(t, os.WriteFile(disk, make([]byte, 8<<20), 0600))
	blob, _, err := machineLayer(context.Background(), []machineTarEntry{{name: "disk.asif", path: disk}})
	require.NoError(t, err)
	before := runtime.NumGoroutine()
	body, err := blob.open()
	require.NoError(t, err)
	_, err = io.CopyN(io.Discard, body, 16)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	// Polled directly: require.Eventually runs its condition in another goroutine.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), before, "the layer generator exits when the upload is abandoned")
}
