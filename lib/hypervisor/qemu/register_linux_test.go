//go:build linux

package qemu

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

// TestMicroVMRegistrationMatchesBoardSupport pins that qemu-microvm's presence
// in the capability registry tracks the same machine-type resolver that gates
// launches, so the registry never advertises a board the host cannot boot.
func TestMicroVMRegistrationMatchesBoardSupport(t *testing.T) {
	t.Parallel()

	_, qemuRegistered := hypervisor.CapabilitiesForType(hypervisor.TypeQEMU)
	require.True(t, qemuRegistered, "standard qemu must register on Linux")

	_, boardErr := resolveMachineTypeForPlatform(MachineTypeMicroVM, runtime.GOOS, runtime.GOARCH)
	_, microVMRegistered := hypervisor.CapabilitiesForType(hypervisor.TypeQEMUMicroVM)
	require.Equal(t, boardErr == nil, microVMRegistered,
		"qemu-microvm registration must match microvm board support on %s/%s", runtime.GOOS, runtime.GOARCH)
}

// TestQEMUAvailabilityTracksLaunchPrerequisites pins that registered QEMU
// runtimes report availability from the full launch-prerequisite check —
// binary resolution, executability/version, and the vhost-vsock host device —
// not just registration: hypeman deliberately starts without a system QEMU
// (cmd/api/main.go only warns) and validates KVM only.
func TestQEMUAvailabilityTracksLaunchPrerequisites(t *testing.T) {
	t.Parallel()

	prereqErr := checkLaunchPrerequisites()
	checked := 0
	for _, rt := range hypervisor.RegisteredRuntimes() {
		if rt.Type != hypervisor.TypeQEMU && rt.Type != hypervisor.TypeQEMUMicroVM {
			continue
		}
		checked++
		require.Equal(t, prereqErr == nil, rt.Available(),
			"%s availability must match the launch-prerequisite check", rt.Type)
		if prereqErr != nil {
			require.Error(t, rt.LaunchErr)
		} else {
			require.NoError(t, rt.LaunchErr)
		}
	}
	require.GreaterOrEqual(t, checked, 1, "standard qemu must be registered on Linux")
}

// TestLaunchCheckCacheSharedByRegistrations pins that both QEMU registry
// entries resolve availability through the same shared cache instance with
// the short production TTL, so one registry read (which checks both boards)
// executes at most one probe per TTL window while repaired prerequisites
// still surface promptly.
func TestLaunchCheckCacheSharedByRegistrations(t *testing.T) {
	t.Parallel()
	require.NotNil(t, launchPrereqCache)
	require.Equal(t, launchCheckCacheTTL, launchPrereqCache.ttl)
}

// fakeQEMUBinary writes an executable script that mimics `qemu --version`
// output, standing in for a working system QEMU.
func fakeQEMUBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-system-fake")
	script := "#!/bin/sh\necho 'QEMU emulator version 8.2.0 (fake)'\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// fakeVsockDevice creates a stand-in for /dev/vhost-vsock: the prerequisite
// check only requires the device node to exist.
func fakeVsockDevice(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "vhost-vsock")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	return path
}

// TestCheckLaunchPrerequisitesFor covers the unavailable cases GetBinaryPath's
// bare os.Stat lookup cannot see: non-executable binaries, binaries that fail
// to run or report an unparseable version, and a missing vhost-vsock device —
// each of which makes every ordinary QEMU launch fail and must therefore
// report unavailable.
func TestCheckLaunchPrerequisitesFor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("working binary and device pass", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, checkLaunchPrerequisitesFor(ctx, fakeQEMUBinary(t, dir), fakeVsockDevice(t, dir)))
	})

	t.Run("missing binary fails", func(t *testing.T) {
		dir := t.TempDir()
		err := checkLaunchPrerequisitesFor(ctx, filepath.Join(dir, "missing"), fakeVsockDevice(t, dir))
		require.ErrorContains(t, err, "not executable")
	})

	t.Run("non-executable binary fails", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "qemu-system-fake")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho 'QEMU emulator version 8.2.0'\n"), 0o644))
		err := checkLaunchPrerequisitesFor(ctx, path, fakeVsockDevice(t, dir))
		require.ErrorContains(t, err, "not executable")
	})

	t.Run("broken binary fails", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "qemu-system-fake")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755))
		err := checkLaunchPrerequisitesFor(ctx, path, fakeVsockDevice(t, dir))
		require.ErrorContains(t, err, "not usable")
	})

	t.Run("unparseable version fails", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "qemu-system-fake")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho 'not qemu'\n"), 0o755))
		err := checkLaunchPrerequisitesFor(ctx, path, fakeVsockDevice(t, dir))
		require.ErrorContains(t, err, "not usable")
	})

	t.Run("missing vsock device fails", func(t *testing.T) {
		dir := t.TempDir()
		err := checkLaunchPrerequisitesFor(ctx, fakeQEMUBinary(t, dir), filepath.Join(dir, "no-vhost-vsock"))
		require.ErrorContains(t, err, "vsock device")
		require.ErrorContains(t, err, "vhost_vsock")
	})

	t.Run("hung binary fails at the context deadline", func(t *testing.T) {
		// A wedged QEMU binary must fail the prerequisite check when its
		// bounded context expires instead of blocking the capability request
		// (and leaking a subprocess) indefinitely. `exec sleep` replaces the
		// shell so this covers the direct-child case; descendant cleanup is
		// pinned by TestVersionFromBinaryKillsProcessGroupOnTimeout, which
		// asserts versionFromBinary's group kill reaps the whole tree.
		dir := t.TempDir()
		path := filepath.Join(dir, "qemu-system-fake")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755))
		shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := checkLaunchPrerequisitesFor(shortCtx, path, fakeVsockDevice(t, dir))
		require.ErrorContains(t, err, "not usable")
		require.Less(t, time.Since(start), 10*time.Second,
			"a hung probe must return at the deadline, not run to completion")
	})
}
