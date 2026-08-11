package ingress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestIngressStorageRejectsPathTraversal(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	p := paths.New(dataDir)
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	outside := filepath.Join(dataDir, "outside.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"id":"outside"}`), 0644))

	_, err := loadIngress(p, "../outside")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, saveIngress(p, &storedIngress{ID: "../outside"}), paths.ErrInvalidPathComponent)
	require.ErrorIs(t, deleteIngressData(p, "../outside"), ErrNotFound)
	require.FileExists(t, outside)
}
