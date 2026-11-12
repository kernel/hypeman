//go:build jailer_integration

package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests boot real guests. They need root, /dev/kvm, and:
//
//	HYPEMAN_TEST_KERNEL     a vmlinux
//	HYPEMAN_HEARTBEAT_ROOTFS an ext4 image whose /sbin/init is scripts/jailer-smoke/heartbeat-init.c
//	HYPEMAN_TEST_DATA_DIR   optional; must share a filesystem with the kernel and rootfs
//	HYPEMAN_TEST_FIRECRACKER and HYPEMAN_JAILER_BINARY
//	                        optional; external binaries instead of the embedded ones
//
// Run with: go test -tags jailer_integration -run Jailed ./lib/hypervisor/firecracker/
const jailedTestUID, jailedTestGID = 2001, 2001

type jailerTestEnv struct {
	jailer, firecracker, kernel, rootfs string
	p                                   *paths.Paths
}

// newJailerTestEnv reads the host setup from the environment and gives the test
// a fresh data directory. It skips the test if anything is missing.
func newJailerTestEnv(t *testing.T) jailerTestEnv {
	t.Helper()
	need := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Skipf("%s not set", name)
		}
		return v
	}
	env := jailerTestEnv{
		jailer:      os.Getenv("HYPEMAN_JAILER_BINARY"),
		firecracker: os.Getenv("HYPEMAN_TEST_FIRECRACKER"),
		kernel:      need("HYPEMAN_TEST_KERNEL"),
		rootfs:      need("HYPEMAN_HEARTBEAT_ROOTFS"),
	}
	base := os.Getenv("HYPEMAN_TEST_DATA_DIR")
	if base == "" {
		base = t.TempDir()
	}
	dataDir, err := os.MkdirTemp(base, "jailer-it-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	env.p = paths.New(dataDir)

	SetCustomBinaryPath(env.firecracker)
	t.Cleanup(func() {
		SetCustomBinaryPath("")
		SetJailerConfig(JailerConfig{})
	})
	return env
}

// config returns a VM with a read-only rootfs, a writable overlay, and a serial log.
func (env jailerTestEnv) config(t *testing.T, id string) hypervisor.VMConfig {
	t.Helper()
	require.NoError(t, os.MkdirAll(env.p.InstanceLogs(id), 0755))
	overlay := env.p.InstanceOverlay(id)
	f, err := os.Create(overlay)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(16<<20))
	require.NoError(t, f.Close())
	return hypervisor.VMConfig{
		VCPUs:         1,
		MemoryBytes:   256 << 20,
		KernelPath:    env.kernel,
		KernelArgs:    "console=ttyS0 reboot=k panic=1 pci=off quiet i8042.noaux i8042.nomux i8042.dumbkbd",
		Disks:         []hypervisor.DiskConfig{{Path: env.rootfs, Readonly: true}, {Path: overlay}},
		SerialLogPath: env.p.InstanceAppLog(id),
	}
}

// TestStandbySnapshotRestore takes two snapshots (full, then diff) of a guest,
// restores after each, and checks that the guest's counter continues. It runs
// jailed, and then unjailed with the flag on, so an unjailed standby VM is shown
// to survive the flag.
func TestStandbySnapshotRestore(t *testing.T) {
	for _, jailed := range []bool{true, false} {
		jailed := jailed
		t.Run(map[bool]string{true: "jailed", false: "unjailed_flag_on"}[jailed], func(t *testing.T) {
			// Each run gets its own data dir: both use the same instance id.
			runStandbySnapshotRestore(t, newJailerTestEnv(t), jailed)
		})
	}
}

func runStandbySnapshotRestore(t *testing.T, env jailerTestEnv, jailed bool) {
	const id = "standby"
	cfg := env.config(t, id)
	starter := NewStarter()
	socketPath := env.p.InstanceSocket(id, starter.SocketName())
	snapDir := env.p.InstanceSnapshotLatest(id)
	ctx := context.Background()

	// The flag is read at start. An unjailed VM started before the flag is set
	// must still restore after it.
	SetJailerConfig(JailerConfig{Enabled: jailed, BinaryPath: env.jailer, UID: jailedTestUID, GID: jailedTestGID})
	pid, hv, err := starter.StartVM(ctx, env.p, "v1.17.0", socketPath, cfg)
	require.NoError(t, err)
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	last := waitForHeartbeat(t, env.p, id, -1)
	requireSocketOwner(t, socketPath, pid)
	if jailed {
		// The VMM runs as the jail uid with no capabilities, and its API socket
		// and inputs are links into the chroot.
		assert.Equal(t, strconv.Itoa(jailedTestUID), procStatusField(t, pid, "Uid:", 1))
		assert.Equal(t, "0000000000000000", procStatusField(t, pid, "CapEff:", 1))
		info, err := os.Lstat(socketPath)
		require.NoError(t, err)
		assert.True(t, info.Mode()&os.ModeSymlink != 0, "api socket should be a link into the jail")
		fcBinary, err := starter.GetBinaryPath(env.p, "v1.17.0")
		require.NoError(t, err)
		fcBinary, err = filepath.EvalSymlinks(fcBinary)
		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(jailRoot(env.p.InstanceDir(id), fcBinary), "vmlinux"))
	}
	for cycle := 0; cycle < 2; cycle++ {
		require.NoError(t, hv.Pause(ctx))
		require.NoError(t, hv.Snapshot(ctx, snapDir), "cycle %d", cycle)
		meta, err := loadRestoreMetadata(env.p.InstanceDir(id))
		require.NoError(t, err)
		require.Equal(t, jailed, meta.Jail != nil, "restore metadata must record how the VM was started")
		if cycle == 1 {
			assert.FileExists(t, filepath.Join(snapDir, "memory"), "the second snapshot is a diff over the first")
		}

		require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
		killed = true

		SetJailerConfig(JailerConfig{Enabled: true, BinaryPath: env.jailer, UID: jailedTestUID, GID: jailedTestGID})
		pid, hv, err = starter.RestoreVM(ctx, env.p, "v1.17.0", socketPath, snapDir)
		require.NoError(t, err, "restore cycle %d", cycle)
		killed = false
		require.NoError(t, hv.Resume(ctx))
		if jailed {
			// Restore must not leave a link to the memory file: a second link makes the
			// next standby copy the whole file before its diff.
			assert.Equal(t, uint64(1), linkCount(t, filepath.Join(snapDir, "memory")), "cycle %d", cycle)
		}

		requireSocketOwner(t, socketPath, pid)
		next := waitForHeartbeat(t, env.p, id, last)
		assert.Greater(t, next, last, "cycle %d: counter must continue, not restart", cycle)
		// The guest counts every 50 ms; 40 ticks is 2 s of guest time.
		assert.LessOrEqual(t, next, last+40, "cycle %d: unexpected gap in the counter", cycle)
		last = next
	}
}

// TestJailedSocketOwners runs two jailed VMs at once. Both bind /api.sock in
// their own jail, and each host socket path must resolve to its own VMM.
func TestJailedSocketOwners(t *testing.T) {
	env := newJailerTestEnv(t)
	SetJailerConfig(JailerConfig{Enabled: true, BinaryPath: env.jailer, UID: jailedTestUID, GID: jailedTestGID})
	starter := NewStarter()
	pids := map[string]int{}
	for _, id := range []string{"owner-a", "owner-b"} {
		socketPath := env.p.InstanceSocket(id, starter.SocketName())
		pid, _, err := starter.StartVM(context.Background(), env.p, "v1.17.0", socketPath, env.config(t, id))
		require.NoError(t, err)
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		pids[socketPath] = pid
	}
	for socketPath, pid := range pids {
		requireSocketOwner(t, socketPath, pid)
	}
}

func requireSocketOwner(t *testing.T, socketPath string, pid int) {
	t.Helper()
	got, err := hypervisor.ResolveProcessPID(socketPath)
	require.NoError(t, err)
	require.Equal(t, pid, got, "socket owner of %s", socketPath)
}

// heartbeatAbove returns the highest "HB n" value in the app or VMM log that is above after, or -1.
func heartbeatAbove(p *paths.Paths, id string, after int64) int64 {
	best := int64(-1)
	for _, f := range []string{p.InstanceAppLog(id), p.InstanceVMMLog(id)} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			var n int64
			if _, err := fmt.Sscanf(strings.TrimSpace(l), "HB %d", &n); err == nil && n > best {
				best = n
			}
		}
	}
	if best > after {
		return best
	}
	return -1
}

// waitForHeartbeat waits for a heartbeat above after and returns the highest value seen.
func waitForHeartbeat(t *testing.T, p *paths.Paths, id string, after int64) int64 {
	t.Helper()
	var best int64
	require.Eventually(t, func() bool {
		best = heartbeatAbove(p, id, after)
		return best >= 0
	}, 30*time.Second, 20*time.Millisecond, "guest heartbeat did not advance past %d", after)
	return best
}

// procStatusField returns field idx of the /proc/<pid>/status line starting with key.
func procStatusField(t *testing.T, pid int, key string, idx int) string {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, key) {
			fields := strings.Fields(line)
			require.Greater(t, len(fields), idx)
			return fields[idx]
		}
	}
	t.Fatalf("status key %s not found", key)
	return ""
}
