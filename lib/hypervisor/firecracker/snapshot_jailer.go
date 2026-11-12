package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
)

// Jailed snapshots. Firecracker can read and write only inside its chroot, so
// the host snapshot files are hardlinked into the jail for the duration of a
// snapshot or restore and moved back out afterwards. The snapshot directory
// records whether it was taken jailed. Restore uses that record, not the
// global jailer flag, so unjailed standby snapshots keep their current behavior.

const (
	// jailedSnapshotDirName is the staging directory inside the chroot.
	jailedSnapshotDirName = "snapshot"
	// jailedSnapshotDir is the same directory as firecracker sees it.
	jailedSnapshotDir = "/" + jailedSnapshotDirName
	// jailedSnapshotMarker is written into a snapshot directory taken from a jailed VM.
	jailedSnapshotMarker = "jailed"
)

// jailRootForSocket returns the chroot root of a jailed VM. Its API socket is a
// symlink to <root>/api.sock.
func jailRootForSocket(socketPath string) (string, error) {
	target, err := os.Readlink(socketPath)
	if err != nil {
		return "", fmt.Errorf("read jailed api socket link: %w", err)
	}
	return filepath.Dir(target), nil
}

// jailOwner returns the uid and gid the jailer gave the chroot root.
func jailOwner(root string) (int, int, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, 0, fmt.Errorf("stat jail root: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("jail root has no unix ownership")
	}
	return int(st.Uid), int(st.Gid), nil
}

func snapshotIsJailed(snapshotDir string) bool {
	_, err := os.Stat(filepath.Join(snapshotDir, jailedSnapshotMarker))
	return err == nil
}

func setSnapshotJailedMarker(snapshotDir string, jailed bool) error {
	marker := filepath.Join(snapshotDir, jailedSnapshotMarker)
	if !jailed {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear jailed snapshot marker: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(marker, nil, 0644); err != nil {
		return fmt.Errorf("write jailed snapshot marker: %w", err)
	}
	return nil
}

// snapshotJailed takes a snapshot of a jailed VM into destPath.
func (f *Firecracker) snapshotJailed(ctx context.Context, destPath string) error {
	root, err := jailRootForSocket(f.socketPath)
	if err != nil {
		return err
	}
	uid, gid, err := jailOwner(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}

	stage := filepath.Join(root, jailedSnapshotDirName)
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("clear jail snapshot staging: %w", err)
	}
	if err := os.MkdirAll(stage, 0755); err != nil {
		return fmt.Errorf("create jail snapshot staging: %w", err)
	}
	// firecracker creates the state file here, so the jail uid must own the directory.
	if err := os.Chown(stage, uid, gid); err != nil {
		return fmt.Errorf("chown jail snapshot staging: %w", err)
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
		MemFilePath:  filepath.Join(jailedSnapshotDir, snapshotMemoryFile),
		SnapshotPath: filepath.Join(jailedSnapshotDir, snapshotStateFile),
		SnapshotType: snapshotType,
	}
	if _, err := f.do(ctx, http.MethodPut, "/snapshot/create", params, http.StatusNoContent); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	for _, name := range []string{snapshotStateFile, snapshotMemoryFile} {
		src := filepath.Join(stage, name)
		if _, err := os.Lstat(src); err != nil {
			return fmt.Errorf("snapshot did not produce %s: %w", name, err)
		}
		dst := filepath.Join(destPath, name)
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move snapshot %s out of jail: %w", name, err)
		}
		// Files firecracker created are owned by the jail uid. Hand them back to the host process.
		if err := os.Chown(dst, os.Getuid(), os.Getgid()); err != nil {
			return fmt.Errorf("chown snapshot %s: %w", name, err)
		}
	}
	return setSnapshotJailedMarker(destPath, true)
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

// restoreJailedVM starts a jailed firecracker and loads a jailed snapshot.
func (s *Starter) restoreJailedVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	jc := getJailerConfig()
	if !jc.Enabled {
		return 0, nil, fmt.Errorf("restore jailed snapshot with jailer disabled: %w", hypervisor.ErrNotSupported)
	}
	instanceDir := filepath.Dir(socketPath)
	meta, err := loadRestoreMetadata(instanceDir)
	if err != nil {
		return 0, nil, fmt.Errorf("load firecracker restore metadata: %w", err)
	}
	if meta.Jail == nil {
		return 0, nil, errors.New("jailed snapshot has no recorded jail inputs")
	}
	if meta.SnapshotSourceDataDir != "" {
		return 0, nil, fmt.Errorf("restore jailed fork: %w", hypervisor.ErrNotSupported)
	}

	binaryPath, err := s.GetBinaryPath(p, version)
	if err != nil {
		return 0, nil, fmt.Errorf("resolve firecracker binary: %w", err)
	}
	jl, err := newJailLaunch(socketPath, binaryPath, *meta.Jail, jc)
	if err != nil {
		return 0, nil, fmt.Errorf("plan firecracker jail: %w", err)
	}
	jl.snapshotDir = snapshotPath
	// The staging links are needed only while firecracker loads the snapshot.
	// A link left behind keeps the memory file's link count above one, and the
	// next standby then copies the whole file before its diff.
	defer func() { _ = os.RemoveAll(filepath.Join(jl.root, jailedSnapshotDirName)) }()

	pid, err := startProcess(binaryPath, socketPath, jl)
	if err != nil {
		return 0, nil, fmt.Errorf("start firecracker process: %w", err)
	}
	restored := false
	defer func() {
		if !restored {
			cleanupFailedRestore(pid, socketPath)
		}
	}()

	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create firecracker client: %w", err)
	}
	if err := hv.loadSnapshot(ctx, jailedSnapshotDir, meta.NetworkOverrides, fileSnapshotMemBackend(jailedSnapshotDir)); err != nil {
		return 0, nil, fmt.Errorf("load firecracker snapshot: %w", err)
	}
	restored = true
	return pid, hv, nil
}
