package qemu

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestWaitForPreviousSWTPMRejectsExpiredDeadline(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "swtpm.pid")
	identity, alive, err := swtpmProcessIdentity(os.Getpid())
	require.NoError(t, err)
	require.True(t, alive)
	require.NoError(t, os.WriteFile(recordPath, []byte(fmt.Sprintf("%d %s\n", os.Getpid(), identity)), 0600))

	err = waitForPreviousSWTPM(&hypervisor.TPMConfig{}, recordPath, time.Now().Add(-time.Second))
	require.ErrorContains(t, err, "timeout waiting for previous swtpm process")
}
