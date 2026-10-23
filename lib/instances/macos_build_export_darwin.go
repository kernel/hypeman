//go:build darwin

package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/images"
)

// StopMacOSBuildInstance performs only the strict graceful stop used by build
// export. It copies no storage and never falls back to a forced hypervisor stop.
// Success requires the shutdown acknowledgement and confirmed VMM exit.
func (m *manager) StopMacOSBuildInstance(ctx context.Context, id string) (receipt MacOSBuildExportReceipt, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return receipt, fmt.Errorf("machine stop requires a deadline")
	}
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	current, err := m.currentInstanceWithoutHydration(ctx, id)
	if err != nil {
		return receipt, err
	}
	if current.MacOS == nil || !current.GuestAgentEnabled() || (current.State != StateRunning && current.State != StateInitializing) {
		return receipt, fmt.Errorf("strict stop requires an active macOS guest with a system agent")
	}
	if err = m.markRestartManualStopLocked(ctx, id); err != nil {
		return
	}
	stopped, err := m.stopInstanceWithFallback(ctx, id, true)
	if err != nil {
		return receipt, err
	}
	receipt = MacOSBuildExportReceipt{Graceful: true, VMMExited: true}
	m.notifyLifecycleEvent(ctx, LifecycleEventStop, stopped)
	return receipt, nil
}

// ExportStoppedMacOSBuildInstance copies closed storage for an instance that is
// still Stopped when the copy starts. The instance lock excludes Start during the
// check and copy; a concurrently restarted instance fails export instead.
func (m *manager) ExportStoppedMacOSBuildInstance(ctx context.Context, id, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return fmt.Errorf("machine export requires a deadline")
	}
	if !filepath.IsAbs(destination) {
		return fmt.Errorf("export destination must be absolute")
	}
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	current, err := m.currentInstanceWithoutHydration(ctx, id)
	if err != nil {
		return err
	}
	if current.State != StateStopped || current.MacOS == nil {
		return fmt.Errorf("export requires a stopped macOS instance")
	}
	if rel, e := filepath.Rel(current.DataDir, destination); e != nil || filepath.IsLocal(rel) {
		return fmt.Errorf("export must be outside instance storage")
	}
	return writeStoppedMacOSBuildBundle(ctx, current, m.paths.InstanceMacOSBootDisk(id), destination)
}

func writeStoppedMacOSBuildBundle(ctx context.Context, inst *Instance, disk, destination string) error {
	if inst.State != StateStopped || inst.MacOS == nil {
		return fmt.Errorf("machine must be stopped")
	}
	aux := filepath.Join(inst.DataDir, "mac-aux.img")
	var diskInfo os.FileInfo
	for _, source := range []string{disk, aux} {
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("machine storage must be nonempty regular files")
		}
		if diskInfo != nil && os.SameFile(diskInfo, info) {
			return fmt.Errorf("machine storage files must be distinct")
		}
		diskInfo = info
		err = exec.CommandContext(ctx, "/usr/sbin/lsof", "-t", source).Run()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 1 {
			return fmt.Errorf("cannot confirm closed machine storage")
		}
	}
	for _, file := range []struct{ source, name string }{{disk, "disk.img"}, {aux, "aux.img"}} {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(destination, file.name)
		if err := forkvm.CopyRegularFile(file.source, path); err != nil {
			return err
		}
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
	}
	data, err := json.Marshal(inst.MacOS)
	if err != nil {
		return err
	}
	path := filepath.Join(destination, "config.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	_, err = images.ValidateMacOSBundle(destination)
	return err
}
