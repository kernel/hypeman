package firecracker

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

// Jailed snapshots. Firecracker can read and write only inside its chroot, so
// the host snapshot files are hardlinked into the jail for the duration of a
// snapshot or restore and moved back out afterwards. The restore metadata's Jail
// field, written on every boot, records whether the VM runs jailed. Snapshot,
// restore and fork use it rather than the global jailer flag, so unjailed
// standby snapshots keep their current behavior.

// snapshotJailed takes a snapshot of a jailed VM into destPath.
func (f *Firecracker) snapshotJailed(ctx context.Context, destPath string) error {
	// The API socket links to <root>/api.sock, and the jailer gave root to the jail uid.
	target, err := os.Readlink(f.socketPath)
	if err != nil {
		return fmt.Errorf("read jailed api socket link: %w", err)
	}
	root := filepath.Dir(target)
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat jail root: %w", err)
	}
	st := info.Sys().(*syscall.Stat_t)
	uid, gid := int(st.Uid), int(st.Gid)
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	stage, err := makeJailStage(root, uid, gid)
	if err != nil {
		return err
	}
	// Staging links only. The host files are moved or linked back out, so
	// removing the directory never deletes snapshot data.
	defer os.RemoveAll(stage)

	// An existing memory file means firecracker writes a diff in place. The
	// link keeps that write visible on the host, as it is for unjailed standby.
	hostMem := snapshotMemoryPath(destPath)
	snapshotType := "Full"
	if _, err := os.Stat(hostMem); err == nil {
		snapshotType = "Diff"
		if err := linkIntoJail(hostMem, filepath.Join(stage, snapshotMemoryFile), true, uid, gid); err != nil {
			return err
		}
	}

	params := snapshotCreateParams{
		MemFilePath:  snapshotMemoryPath("/" + jailedSnapshotDir),
		SnapshotPath: snapshotStatePath("/" + jailedSnapshotDir),
		SnapshotType: snapshotType,
	}
	if _, err := f.do(ctx, http.MethodPut, "/snapshot/create", params, http.StatusNoContent); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	for _, name := range []string{snapshotStateFile, snapshotMemoryFile} {
		dst := filepath.Join(destPath, name)
		if err := os.Rename(filepath.Join(stage, name), dst); err != nil {
			return fmt.Errorf("move snapshot %s out of jail: %w", name, err)
		}
		// Files firecracker created are owned by the jail uid. Hand them back to the host process.
		if err := os.Chown(dst, os.Getuid(), os.Getgid()); err != nil {
			return fmt.Errorf("chown snapshot %s: %w", name, err)
		}
	}
	return nil
}

// recordJailInputs stores the jail inputs in the instance's restore metadata.
func recordJailInputs(instanceDir string, in jailInputs) error {
	meta, err := loadRestoreMetadata(instanceDir)
	if err != nil {
		return err
	}
	meta.Jail = &in
	return saveRestoreMetadataState(instanceDir, meta)
}
