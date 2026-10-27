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
	bundle := filepath.Join(parent, "bundle")
	require.NoError(t, os.Mkdir(bundle, 0700))
	disk := make([]byte, 3<<20) // mostly zeros, like a sparse machine disk
	copy(disk[1<<20:], "data")
	for name, data := range map[string][]byte{"disk.img": disk, "aux.img": []byte("aux"), "config.json": []byte("{}")} {
		require.NoError(t, os.WriteFile(filepath.Join(bundle, name), data, 0600))
	}

	layer, err := machineTarballLayer(context.Background(), bundle)
	require.NoError(t, err)
	diffID, err := layer.DiffID()
	require.NoError(t, err)
	require.NotEmpty(t, diffID.Hex)
	digest, err := layer.Digest()
	require.NoError(t, err)
	size, err := layer.Size()
	require.NoError(t, err)
	for range 2 {
		body, err := layer.Compressed()
		require.NoError(t, err)
		sum := sha256.New()
		n, err := io.Copy(sum, body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		require.Equal(t, size, n)
		require.Equal(t, digest.Hex, hex.EncodeToString(sum.Sum(nil)))
	}
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing is spooled beside the bundle")
}

func TestMachineLayerAbandonedUploadStopsGenerator(t *testing.T) {
	bundle := t.TempDir()
	for name, data := range map[string][]byte{"disk.img": make([]byte, 8<<20), "aux.img": []byte("aux"), "config.json": []byte("{}")} {
		require.NoError(t, os.WriteFile(filepath.Join(bundle, name), data, 0600))
	}
	layer, err := machineTarballLayer(context.Background(), bundle)
	require.NoError(t, err)
	before := runtime.NumGoroutine()
	body, err := layer.Compressed()
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
