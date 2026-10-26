package instances

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSAppLogSourceIsNotSupported(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	require.NoError(t, m.ensureDirectories("mac-logs"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: "mac-logs", DataDir: p.InstanceDir("mac-logs"), MacOS: &images.MacOSImage{}}}))
	_, err := m.streamInstanceLogs(context.Background(), "mac-logs", 10, false, LogSourceApp)
	require.ErrorIs(t, err, ErrNotSupported)
}
