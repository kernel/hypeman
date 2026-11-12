package firecracker

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJailedVMConfigRewritesOnlyHostPaths(t *testing.T) {
	host := hypervisor.VMConfig{
		VCPUs:         2,
		KernelPath:    "/host/vmlinux",
		InitrdPath:    "/host/initrd",
		Disks:         []hypervisor.DiskConfig{{Path: "/host/rootfs", Readonly: true}, {Path: "/host/overlay"}},
		VsockCID:      3,
		VsockSocket:   "/host/instance/vsock.sock",
		SerialLogPath: "/host/instance/logs/app.log",
	}

	got := jailedVMConfig(host)

	assert.Equal(t, "/vmlinux", got.KernelPath)
	assert.Equal(t, "/initrd", got.InitrdPath)
	assert.Equal(t, []hypervisor.DiskConfig{
		{Path: "/disk0", Readonly: true},
		{Path: "/disk1"},
	}, got.Disks)
	assert.Equal(t, "/vsock.sock", got.VsockSocket)
	assert.Equal(t, "/serial.log", got.SerialLogPath)
	assert.Equal(t, 2, got.VCPUs, "non-path fields are preserved")

	// The caller's config must not change.
	assert.Equal(t, "/host/vmlinux", host.KernelPath)
	assert.Equal(t, "/host/rootfs", host.Disks[0].Path)
}

func TestJailLaunchPrepare(t *testing.T) {
	instanceDir := t.TempDir()
	socketPath := filepath.Join(instanceDir, "fc.sock")
	fcBinary := writeFile(t, filepath.Join(t.TempDir(), "firecracker"), "fc")
	kernel := writeFile(t, filepath.Join(t.TempDir(), "vmlinux"), "kernel")
	rootfs := writeFile(t, filepath.Join(t.TempDir(), "rootfs"), "rootfs")
	overlay := writeFile(t, filepath.Join(instanceDir, "overlay.raw"), "overlay")
	serial := filepath.Join(instanceDir, "logs", "app.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(serial), 0755))
	vsock := filepath.Join(instanceDir, "vsock.sock")
	snapshotDir := filepath.Join(instanceDir, "snapshots", "snapshot-latest")
	state := writeFile(t, filepath.Join(snapshotDir, "state"), "state")
	mem := writeFile(t, filepath.Join(snapshotDir, "memory"), "mem")

	// Use the test process's own uid/gid so chown succeeds without root.
	jc := JailerConfig{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	in := jailInputs{
		KernelPath:    kernel,
		Disks:         []jailDisk{{Path: rootfs, Readonly: true}, {Path: overlay}},
		VsockSocket:   vsock,
		SerialLogPath: serial,
	}

	jl, err := newJailLaunch(socketPath, fcBinary, "/usr/bin/jailer", in, jc)
	require.NoError(t, err)
	jl.snapshotDir = snapshotDir
	require.NoError(t, jl.prepare(socketPath))

	root := jailRoot(instanceDir, fcBinary)
	assertSameInode(t, kernel, filepath.Join(root, "vmlinux"))
	assertSameInode(t, rootfs, filepath.Join(root, "disk0"))
	assertSameInode(t, overlay, filepath.Join(root, "disk1"))
	assertSameInode(t, state, filepath.Join(root, "snapshot", "state"))
	assertSameInode(t, mem, filepath.Join(root, "snapshot", "memory"))

	// Writable inputs are chowned to the jail uid; read-only ones are not touched.
	assertOwner(t, filepath.Join(root, "disk1"), os.Getuid())

	// Host-visible sockets and logs point into the jail.
	assertSymlinkTo(t, socketPath, filepath.Join(root, "api.sock"))
	assertSymlinkTo(t, vsock, filepath.Join(root, "vsock.sock"))
	assertSymlinkTo(t, serial, filepath.Join(root, "serial.log"))

	args := jl.args()
	assert.Equal(t, []string{
		"--id", "vm",
		"--exec-file", fcBinary,
		"--uid", strconv.Itoa(os.Getuid()),
		"--gid", strconv.Itoa(os.Getgid()),
		"--chroot-base-dir", filepath.Join(instanceDir, "jail"),
		"--", "--api-sock", "/api.sock",
	}, args)
}

func TestJailRootFollowsResolvedFirecrackerBinary(t *testing.T) {
	instanceDir := t.TempDir()
	binDir := t.TempDir()
	target := writeFile(t, filepath.Join(binDir, "firecracker-v1.17.0-x86_64"), "fc")
	link := filepath.Join(binDir, "firecracker")
	require.NoError(t, os.Symlink(target, link))

	jl, err := newJailLaunch(filepath.Join(instanceDir, "fc.sock"), link, "/usr/bin/jailer", jailInputs{}, JailerConfig{})
	require.NoError(t, err)

	// The jailer canonicalizes --exec-file and names the chroot after the result.
	assert.Equal(t, filepath.Join(instanceDir, "jail", "firecracker-v1.17.0-x86_64", "vm", "root"), jl.root)
	assert.Contains(t, jl.args(), target)
}

func TestResolveJailerPathNeedsOverrideWithCustomFirecracker(t *testing.T) {
	SetCustomBinaryPath(writeFile(t, filepath.Join(t.TempDir(), "firecracker"), "fc"))
	t.Cleanup(func() { SetCustomBinaryPath("") })

	_, err := resolveJailerPath(paths.New(t.TempDir()), "", JailerConfig{Enabled: true})
	require.Error(t, err, "the embedded jailer may not match a custom firecracker")
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func assertSameInode(t *testing.T, a, b string) {
	t.Helper()
	ia, err := os.Stat(a)
	require.NoError(t, err)
	ib, err := os.Stat(b)
	require.NoError(t, err)
	assert.True(t, os.SameFile(ia, ib), "%s and %s should be the same file", a, b)
}

func assertSymlinkTo(t *testing.T, link, target string) {
	t.Helper()
	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, target, got)
}

func assertOwner(t *testing.T, path string, uid int) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(uid), st.Uid)
}
