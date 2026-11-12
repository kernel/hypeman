package firecracker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortTempDir keeps unix socket paths under the 108-byte limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fcj")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveFakeFirecracker answers PUT /snapshot/create on sockPath. It calls write
// with the parsed params so the test can act as firecracker writing files.
func serveFakeFirecracker(t *testing.T, sockPath string, write func(snapshotCreateParams)) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot/create", func(w http.ResponseWriter, r *http.Request) {
		var params snapshotCreateParams
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		write(params)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// jailedFixture returns an instance dir with recorded jail inputs, a jailed
// firecracker socket symlink, and the chroot root behind it.
func jailedFixture(t *testing.T) (instanceDir, socketPath, root string) {
	t.Helper()
	instanceDir = shortTempDir(t)
	fcBinary := writeFile(t, filepath.Join(instanceDir, "bin", "firecracker"), "fc")
	root = jailRoot(instanceDir, fcBinary)
	require.NoError(t, os.MkdirAll(root, 0755))
	socketPath = filepath.Join(instanceDir, "fc.sock")
	require.NoError(t, os.Symlink(filepath.Join(root, "api.sock"), socketPath))
	require.NoError(t, recordJailInputs(instanceDir, jailInputs{KernelPath: "/system/vmlinux"}))
	return instanceDir, socketPath, root
}

func TestJailedSnapshotDiffWritesInPlaceAndMovesStateOut(t *testing.T) {
	_, socketPath, root := jailedFixture(t)
	destPath := filepath.Join(filepath.Dir(root), "snapshot-latest")
	writeFile(t, filepath.Join(destPath, "state"), "state-v1")
	writeFile(t, filepath.Join(destPath, "memory"), "mem-v1")
	memBefore, err := os.Stat(filepath.Join(destPath, "memory"))
	require.NoError(t, err)

	var got snapshotCreateParams
	serveFakeFirecracker(t, filepath.Join(root, "api.sock"), func(p snapshotCreateParams) {
		got = p
		// Firecracker writes inside its chroot, at the in-jail paths it was given.
		require.NoError(t, os.WriteFile(filepath.Join(root, p.SnapshotPath), []byte("state-v2"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(root, p.MemFilePath), []byte("mem-v2"), 0644))
	})

	f, err := New(socketPath)
	require.NoError(t, err)
	require.NoError(t, f.Snapshot(context.Background(), destPath))

	assert.Equal(t, "Diff", got.SnapshotType, "an existing memory file means a diff snapshot")
	assert.Equal(t, "/snapshot/memory", got.MemFilePath)
	assert.Equal(t, "/snapshot/state", got.SnapshotPath)

	assert.Equal(t, "state-v2", readFileString(t, filepath.Join(destPath, "state")))
	assert.Equal(t, "mem-v2", readFileString(t, filepath.Join(destPath, "memory")))
	memAfter, err := os.Stat(filepath.Join(destPath, "memory"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(memBefore, memAfter), "diff writes the base memory in place, as unjailed standby does")

	_, err = os.Stat(filepath.Join(root, jailedSnapshotDirName))
	assert.True(t, os.IsNotExist(err), "staging is removed after the snapshot")
	assert.Equal(t, uint64(1), linkCount(t, filepath.Join(destPath, "memory")), "no staging link is left on the host file")
}

func TestJailedSnapshotFullWhenNoBaseMemory(t *testing.T) {
	_, socketPath, root := jailedFixture(t)
	destPath := filepath.Join(filepath.Dir(root), "snapshot-latest")

	var got snapshotCreateParams
	serveFakeFirecracker(t, filepath.Join(root, "api.sock"), func(p snapshotCreateParams) {
		got = p
		require.NoError(t, os.WriteFile(filepath.Join(root, p.SnapshotPath), []byte("state-1"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(root, p.MemFilePath), []byte("mem-1"), 0644))
	})

	f, err := New(socketPath)
	require.NoError(t, err)
	require.NoError(t, f.Snapshot(context.Background(), destPath))

	assert.Equal(t, "Full", got.SnapshotType)
	assert.Equal(t, "mem-1", readFileString(t, filepath.Join(destPath, "memory")))
	assert.Equal(t, "state-1", readFileString(t, filepath.Join(destPath, "state")))
}

func TestRestoreJailedSnapshotWithJailerDisabledFailsBeforeStart(t *testing.T) {
	SetJailerConfig(JailerConfig{})
	dir := shortTempDir(t)
	snapshotDir := filepath.Join(dir, "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, recordJailInputs(dir, jailInputs{KernelPath: "/system/vmlinux"}))

	_, _, err := NewStarter().RestoreVM(context.Background(), paths.New(dir), "v1.17.0", filepath.Join(dir, "fc.sock"), snapshotDir)
	require.ErrorIs(t, err, hypervisor.ErrNotSupported)
	_, statErr := os.Stat(filepath.Join(dir, "fc.sock"))
	assert.True(t, os.IsNotExist(statErr), "no socket or process should be created")
}

func TestPrepareForkRebasesJailInputsForJailedSnapshot(t *testing.T) {
	source := shortTempDir(t)
	target := shortTempDir(t)
	snapshotDir := filepath.Join(target, "snapshots", "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	// The copied metadata still names the source instance's files.
	require.NoError(t, saveRestoreMetadata(target, nil))
	require.NoError(t, recordJailInputs(target, jailInputs{
		KernelPath:    "/system/vmlinux",
		Disks:         []jailDisk{{Path: "/images/rootfs", Readonly: true}, {Path: filepath.Join(source, "overlay.raw")}},
		VsockSocket:   filepath.Join(source, "vsock.sock"),
		SerialLogPath: filepath.Join(source, "logs", "app.log"),
	}))

	res, err := NewStarter().PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: filepath.Join(snapshotDir, "config.json"),
		SourceDataDir:      source,
		TargetDataDir:      target,
		Network:            &hypervisor.ForkNetworkConfig{TAPDevice: "tap0"},
	})
	require.NoError(t, err)
	assert.False(t, res.RequiresSnapshotSourceAlias, "a jailed fork needs no source alias")

	meta, err := loadRestoreMetadata(target)
	require.NoError(t, err)
	assert.Empty(t, meta.SnapshotSourceDataDir)
	assert.Equal(t, filepath.Join(target, "overlay.raw"), meta.Jail.Disks[1].Path)
	assert.Equal(t, filepath.Join(target, "vsock.sock"), meta.Jail.VsockSocket)
	assert.Equal(t, filepath.Join(target, "logs", "app.log"), meta.Jail.SerialLogPath)
	assert.Equal(t, "/images/rootfs", meta.Jail.Disks[0].Path, "paths outside the source are untouched")
	assert.Equal(t, "/system/vmlinux", meta.Jail.KernelPath)
}

func TestRebaseJailInputsIgnoresSiblingPrefixes(t *testing.T) {
	in := &jailInputs{Disks: []jailDisk{{Path: "/data/guests/a-copy/overlay.raw"}}}
	assert.False(t, rebaseJailInputs(in, "/data/guests/a", "/data/guests/b"), "a-copy is not under a")
	assert.Equal(t, "/data/guests/a-copy/overlay.raw", in.Disks[0].Path)
}

func TestJailPrepareStagesSnapshotForRestore(t *testing.T) {
	instanceDir := shortTempDir(t)
	fcBinary := writeFile(t, filepath.Join(instanceDir, "bin", "firecracker"), "fc")
	snapshotDir := filepath.Join(instanceDir, "snapshots", "snapshot-latest")
	state := writeFile(t, filepath.Join(snapshotDir, "state"), "state")
	mem := writeFile(t, filepath.Join(snapshotDir, "memory"), "mem")
	kernel := writeFile(t, filepath.Join(instanceDir, "vmlinux"), "k")

	jc := JailerConfig{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	socketPath := filepath.Join(instanceDir, "fc.sock")
	jl, err := newJailLaunch(socketPath, fcBinary, "/usr/bin/jailer", jailInputs{KernelPath: kernel}, jc)
	require.NoError(t, err)
	jl.snapshotDir = snapshotDir
	require.NoError(t, jl.prepare(socketPath))

	stage := filepath.Join(jailRoot(instanceDir, fcBinary), jailedSnapshotDirName)
	assertSameInode(t, state, filepath.Join(stage, "state"))
	assertSameInode(t, mem, filepath.Join(stage, "memory"))
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func linkCount(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return uint64(info.Sys().(*syscall.Stat_t).Nlink)
}
