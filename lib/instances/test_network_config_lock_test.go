package instances

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTryWithTestFileLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	called := false
	err := tryWithTestFileLock(lockPath, func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, called)

	heldLock, err := openTestLockFile(lockPath)
	require.NoError(t, err)
	defer heldLock.Close()
	require.NoError(t, syscall.Flock(int(heldLock.Fd()), syscall.LOCK_EX))
	defer syscall.Flock(int(heldLock.Fd()), syscall.LOCK_UN)

	called = false
	err = tryWithTestFileLock(lockPath, func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.False(t, called)
}

func TestReleaseRemovesLeaseBeforeNetworkArtifacts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	const subnet = "10.200.1.0/24"
	require.NoError(t, saveSubnetLeases(map[string]subnetLease{subnet: {SubnetCIDR: subnet}}))

	binDir := filepath.Join(tmpDir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o755))
	markerPath := filepath.Join(tmpDir, "lease-present-during-cleanup")
	t.Setenv("HYPEMAN_TEST_RELEASE_MARKER", markerPath)
	t.Setenv("HYPEMAN_TEST_RELEASE_LEASES", testSubnetLeaseFilePath())
	t.Setenv("HYPEMAN_TEST_RELEASE_SUBNET", subnet)

	ipScript := `#!/bin/sh
if [ "$1" = "-4" ] && [ "$2" = "route" ] && [ "$3" = "del" ]; then
	if grep -Fq "$HYPEMAN_TEST_RELEASE_SUBNET" "$HYPEMAN_TEST_RELEASE_LEASES"; then
		touch "$HYPEMAN_TEST_RELEASE_MARKER"
	fi
fi
exit 0
`
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "ip"), []byte(ipScript), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "iptables"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	releaseTestNetworkLease("hm1234", subnet)

	require.NoFileExists(t, markerPath)
	leases, err := loadSubnetLeases()
	require.NoError(t, err)
	require.NotContains(t, leases, subnet)
}

func TestParseIPTablesAppendRule(t *testing.T) {
	line := `-A FORWARD -i hm1234 -o bond0 -m comment --comment "hypeman-fwd-out-hm1234" -j ACCEPT`
	args, comment, ok := parseIPTablesAppendRule("filter", line)
	require.True(t, ok)
	require.Equal(t, "hypeman-fwd-out-hm1234", comment)
	require.Equal(t, []string{
		"-t", "filter", "-D", "FORWARD", "-i", "hm1234", "-o", "bond0",
		"-m", "comment", "--comment", "hypeman-fwd-out-hm1234", "-j", "ACCEPT",
	}, args)
}
