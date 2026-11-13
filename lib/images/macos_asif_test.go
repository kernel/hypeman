package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConvertRawToASIF(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.img")
	data := make([]byte, 256<<20)
	copy(data, "first chunk")
	copy(data[100<<20+123:], "middle")
	copy(data[len(data)-4:], "tail")
	require.NoError(t, os.WriteFile(raw, data, 0600))
	asif := filepath.Join(dir, "disk.asif")
	require.NoError(t, ConvertRawToASIF(context.Background(), raw, asif))

	img, err := OpenMacOSDiskChain([]string{asif})
	require.NoError(t, err)
	defer img.Close()
	require.Equal(t, int64(len(data)), img.Size())
	got := make([]byte, len(data))
	_, err = img.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, data, got)
	info, err := os.Stat(asif)
	require.NoError(t, err)
	require.Less(t, info.Size(), int64(len(data))/4, "zero chunks are not allocated")
}

func TestConvertRawToASIFRejectsPartialBlocksAndCancellation(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.img")
	require.NoError(t, os.WriteFile(raw, make([]byte, 1000), 0600))
	require.Error(t, ConvertRawToASIF(context.Background(), raw, filepath.Join(dir, "a.asif")))

	require.NoError(t, os.WriteFile(raw, make([]byte, 4<<20), 0600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(dir, "b.asif")
	require.ErrorIs(t, ConvertRawToASIF(ctx, raw, out), context.Canceled)
	require.NoFileExists(t, out, "a failed conversion leaves no image")
}

func TestOpenMacOSDiskChainRejectsUnrelatedOverlay(t *testing.T) {
	_, overlay := macOSFixtureChain(t, t.TempDir())
	otherBase, _ := macOSFixtureChain(t, t.TempDir())
	_, err := OpenMacOSDiskChain([]string{otherBase, overlay})
	require.Error(t, err)
	_, err = OpenMacOSDiskChain(nil)
	require.Error(t, err)
}
