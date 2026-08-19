//go:build linux && amd64

package qemu

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func requireWindowsConfigDependency(t *testing.T, path, description string) string {
	t.Helper()
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	if os.Getenv("CI") == "true" {
		t.Fatalf("required Windows config integration dependency is missing: %s (%s)", description, path)
	}
	t.Skipf("%s is unavailable", description)
	return ""
}

func TestWindowsConfigIntegration(t *testing.T) {
	requireWindowsConfigDependency(t, "/dev/kvm", "KVM")
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		requireWindowsConfigDependency(t, "", "qemu-system-x86_64")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		requireWindowsConfigDependency(t, "", "qemu-img")
	}
	if _, err := exec.LookPath("swtpm"); err != nil {
		requireWindowsConfigDependency(t, "", "swtpm")
	}

	codePath := os.Getenv("HYPEMAN_WINDOWS_OVMF_CODE")
	if codePath == "" {
		codePath = "/usr/share/OVMF/OVMF_CODE_4M.secboot.fd"
	}
	varsTemplate := os.Getenv("HYPEMAN_WINDOWS_OVMF_VARS")
	if varsTemplate == "" {
		varsTemplate = "/usr/share/OVMF/OVMF_VARS_4M.ms.fd"
	}
	requireWindowsConfigDependency(t, codePath, "Secure Boot OVMF code")
	requireWindowsConfigDependency(t, varsTemplate, "Microsoft-enrolled OVMF variables")

	dir, err := os.MkdirTemp("/tmp", "hypeman-win-config-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })

	varsData, err := os.ReadFile(varsTemplate)
	require.NoError(t, err)
	varsPath := filepath.Join(dir, "OVMF_VARS.fd")
	require.NoError(t, os.WriteFile(varsPath, varsData, 0600))

	basePath := filepath.Join(dir, "base.raw")
	base, err := os.Create(basePath)
	require.NoError(t, err)
	require.NoError(t, base.Truncate(64<<20))
	require.NoError(t, base.Close())

	diskPath := filepath.Join(dir, "instance.qcow2")
	output, err := exec.Command("qemu-img", "create", "-f", "qcow2", "-F", "raw", "-b", basePath, diskPath).CombinedOutput()
	require.NoError(t, err, "qemu-img create: %s", output)

	socketPath := filepath.Join(dir, "qemu.sock")
	tpmSocket := filepath.Join(dir, "swtpm.sock")
	tpmState := filepath.Join(dir, "tpm")
	config := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 << 20,
		BootMode:    hypervisor.BootModeUEFI,
		Firmware: &hypervisor.FirmwareConfig{
			CodePath:   codePath,
			VarsPath:   varsPath,
			SecureBoot: true,
		},
		TPM:   &hypervisor.TPMConfig{SocketPath: tpmSocket, StateDir: tpmState},
		Disks: []hypervisor.DiskConfig{{Path: diskPath, Format: hypervisor.DiskFormatQCOW2}},
	}

	starter := NewStarter()
	boot := func() {
		pid, vm, err := starter.StartVM(context.Background(), paths.New(dir), "", socketPath, config)
		require.NoError(t, err)
		require.Positive(t, pid)
		info, err := vm.GetVMInfo(context.Background())
		require.NoError(t, err)
		require.Equal(t, hypervisor.StateRunning, info.State)
		require.NoError(t, vm.Shutdown(context.Background()))
		require.Eventually(t, func() bool {
			_, err := os.Stat(socketPath)
			return os.IsNotExist(err)
		}, 5*time.Second, 20*time.Millisecond)
	}

	boot()
	info, err := os.Stat(diskPath)
	require.NoError(t, err)
	require.Positive(t, info.Size(), "qcow2 overlay must contain metadata")

	var stateFiles int
	require.NoError(t, filepath.WalkDir(tpmState, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			stateFiles++
		}
		return err
	}))
	require.Positive(t, stateFiles, "swtpm must persist TPM 2.0 state")
	require.FileExists(t, varsPath)

	boot()
	require.FileExists(t, varsPath)
}
