package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
)

// JailerConfig runs each firecracker process under the firecracker jailer.
// The VMM runs as UID/GID inside a chroot under its instance directory and can
// open only the files hypeman links into that chroot.
type JailerConfig struct {
	Enabled bool
	// BinaryPath overrides the jailer embedded with each firecracker version.
	BinaryPath string
	UID        int
	GID        int
}

var (
	jailerConfigMu sync.RWMutex
	jailerCfg      JailerConfig
)

// SetJailerConfig sets the process-wide jailer configuration. VMs read it at start.
func SetJailerConfig(cfg JailerConfig) {
	jailerConfigMu.Lock()
	defer jailerConfigMu.Unlock()
	jailerCfg = cfg
}

func getJailerConfig() JailerConfig {
	jailerConfigMu.RLock()
	defer jailerConfigMu.RUnlock()
	return jailerCfg
}

const (
	jailerID          = "vm"
	jailBaseDirName   = paths.InstanceJailDirName
	jailedAPISocket   = "/api.sock"
	jailedVsockSocket = "/vsock.sock"
	jailedSerialLog   = "/serial.log"
	jailedKernel      = "/vmlinux"
	jailedInitrd      = "/initrd"
	jailedVsockName   = "vsock.sock"
	jailedSerialName  = "serial.log"
)

func jailedDiskPath(i int) string {
	return fmt.Sprintf("/disk%d", i)
}

// jailRoot returns the host path of the chroot root. The jailer names the
// root after the basename of the exec file, so fcBinary must already have its
// symlinks resolved, as the jailer does.
func jailRoot(instanceDir, fcBinary string) string {
	return filepath.Join(instanceDir, jailBaseDirName, filepath.Base(fcBinary), jailerID, "root")
}

// jailInputs are the host paths a jailed VM is started with. They are
// persisted with the instance, so restore can re-link the same files under the
// same in-jail names that the snapshot refers to.
type jailInputs struct {
	KernelPath    string     `json:"kernel_path"`
	InitrdPath    string     `json:"initrd_path,omitempty"`
	Disks         []jailDisk `json:"disks"`
	VsockSocket   string     `json:"vsock_socket,omitempty"`
	SerialLogPath string     `json:"serial_log_path,omitempty"`
}

type jailDisk struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

func jailInputsFromConfig(cfg hypervisor.VMConfig) jailInputs {
	in := jailInputs{
		KernelPath:    cfg.KernelPath,
		InitrdPath:    cfg.InitrdPath,
		VsockSocket:   cfg.VsockSocket,
		SerialLogPath: cfg.SerialLogPath,
	}
	for _, d := range cfg.Disks {
		in.Disks = append(in.Disks, jailDisk{Path: d.Path, Readonly: d.Readonly})
	}
	return in
}

// rebaseJailInputs repoints host paths under from at to. It reports whether anything changed.
func rebaseJailInputs(in *jailInputs, from, to string) bool {
	if in == nil {
		return false
	}
	from, to = filepath.Clean(from), filepath.Clean(to)
	changed := false
	rebase := func(p string) string {
		if !strings.HasPrefix(p, from+string(filepath.Separator)) {
			return p
		}
		changed = true
		return to + strings.TrimPrefix(p, from)
	}
	in.VsockSocket = rebase(in.VsockSocket)
	in.SerialLogPath = rebase(in.SerialLogPath)
	for i := range in.Disks {
		in.Disks[i].Path = rebase(in.Disks[i].Path)
	}
	return changed
}

// jailedVMConfig returns cfg with each path firecracker opens replaced by its
// in-jail path. It does not modify cfg.
func jailedVMConfig(cfg hypervisor.VMConfig) hypervisor.VMConfig {
	out := cfg
	out.KernelPath = jailedKernel
	if cfg.InitrdPath != "" {
		out.InitrdPath = jailedInitrd
	}
	out.Disks = make([]hypervisor.DiskConfig, len(cfg.Disks))
	for i, d := range cfg.Disks {
		d.Path = jailedDiskPath(i)
		out.Disks[i] = d
	}
	if cfg.VsockSocket != "" {
		out.VsockSocket = jailedVsockSocket
	}
	if cfg.SerialLogPath != "" {
		out.SerialLogPath = jailedSerialLog
	}
	return out
}

// jailLaunch is the host-side plan for starting a firecracker process under the jailer.
type jailLaunch struct {
	jailerPath  string
	fcBinary    string
	instanceDir string
	root        string
	in          jailInputs
	uid         int
	gid         int
	// snapshotDir, when set, is a host snapshot directory to load on restore.
	// prepare links its state and memory files into the jail.
	snapshotDir string
}

func newJailLaunch(socketPath, fcBinary, jailerPath string, in jailInputs, jc JailerConfig) (*jailLaunch, error) {
	if !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf("jailed firecracker needs an absolute socket path, got %q", socketPath)
	}
	// The jailer canonicalizes --exec-file and names the chroot after the result.
	fcBinary, err := filepath.EvalSymlinks(fcBinary)
	if err != nil {
		return nil, fmt.Errorf("resolve firecracker binary: %w", err)
	}
	instanceDir := filepath.Dir(socketPath)
	return &jailLaunch{
		jailerPath:  jailerPath,
		fcBinary:    fcBinary,
		instanceDir: instanceDir,
		root:        jailRoot(instanceDir, fcBinary),
		in:          in,
		uid:         jc.UID,
		gid:         jc.GID,
	}, nil
}

// prepare stages every file firecracker opens inside the chroot and links the
// host-visible sockets and logs into it. The caller must have confirmed that no
// firecracker process is serving socketPath.
func (jl *jailLaunch) prepare(socketPath string) error {
	if err := os.MkdirAll(jl.root, 0755); err != nil {
		return fmt.Errorf("create jail root: %w", err)
	}
	// Everything from the previous launch goes, except the serial log, so a restore
	// continues the guest's output. A fresh start archives the log first
	// (archiveAppLogForBoot). The jailer creates its device nodes and copies the
	// exec file, and it fails if they already exist.
	entries, err := os.ReadDir(jl.root)
	if err != nil {
		return fmt.Errorf("read jail root: %w", err)
	}
	for _, e := range entries {
		if e.Name() == jailedSerialName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(jl.root, e.Name())); err != nil {
			return fmt.Errorf("clear jail entry %s: %w", e.Name(), err)
		}
	}
	_ = os.Remove(socketPath)

	if err := linkIntoJail(jl.in.KernelPath, filepath.Join(jl.root, jailedKernel), false, jl.uid, jl.gid); err != nil {
		return err
	}
	if jl.in.InitrdPath != "" {
		if err := linkIntoJail(jl.in.InitrdPath, filepath.Join(jl.root, jailedInitrd), false, jl.uid, jl.gid); err != nil {
			return err
		}
	}
	for i, d := range jl.in.Disks {
		if err := linkIntoJail(d.Path, filepath.Join(jl.root, jailedDiskPath(i)), !d.Readonly, jl.uid, jl.gid); err != nil {
			return err
		}
	}
	if jl.in.SerialLogPath != "" {
		// The host log becomes a link to the jail's copy. A log written by an earlier
		// unjailed launch is replaced, which only happens when a VM is moved into the jail.
		if err := replaceWithSymlink(filepath.Join(jl.root, jailedSerialName), jl.in.SerialLogPath); err != nil {
			return fmt.Errorf("link serial log: %w", err)
		}
	}
	if jl.in.VsockSocket != "" {
		if err := replaceWithSymlink(filepath.Join(jl.root, jailedVsockName), jl.in.VsockSocket); err != nil {
			return fmt.Errorf("link vsock socket: %w", err)
		}
	}
	if jl.snapshotDir != "" {
		if err := jl.stageSnapshot(); err != nil {
			return err
		}
	}
	// The API socket is created by firecracker inside the jail. This symlink lets
	// hypeman dial it at the usual instance path.
	if err := replaceWithSymlink(filepath.Join(jl.root, "api.sock"), socketPath); err != nil {
		return fmt.Errorf("link api socket: %w", err)
	}
	return nil
}

// stageSnapshot links a host snapshot's state and memory into the jail. The
// memory file is writable because a later diff snapshot writes it in place.
func (jl *jailLaunch) stageSnapshot() error {
	stage := filepath.Join(jl.root, jailedSnapshotDirName)
	if err := os.MkdirAll(stage, 0755); err != nil {
		return fmt.Errorf("create jail snapshot staging: %w", err)
	}
	if err := os.Chown(stage, jl.uid, jl.gid); err != nil {
		return fmt.Errorf("chown jail snapshot staging: %w", err)
	}
	for _, name := range []string{snapshotStateFile, snapshotMemoryFile} {
		if err := linkIntoJail(filepath.Join(jl.snapshotDir, name), filepath.Join(stage, name), true, jl.uid, jl.gid); err != nil {
			return err
		}
	}
	return nil
}

// args returns the jailer command line. Arguments after "--" go to firecracker.
func (jl *jailLaunch) args() []string {
	return []string{
		"--id", jailerID,
		"--exec-file", jl.fcBinary,
		"--uid", strconv.Itoa(jl.uid),
		"--gid", strconv.Itoa(jl.gid),
		"--chroot-base-dir", filepath.Join(jl.instanceDir, jailBaseDirName),
		"--", "--api-sock", jailedAPISocket,
	}
}

// linkIntoJail hardlinks src into the jail at dst. Hardlinks share the inode, so
// a writable file is chowned to the jail uid for the host copy too.
func linkIntoJail(src, dst string, writable bool, uid, gid int) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("resolve jail input %s: %w", src, err)
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear jail input %s: %w", dst, err)
	}
	if err := os.Link(resolved, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("link jail input %s: jailed VMs need inputs on the same filesystem as the instance directory: %w", src, err)
		}
		return fmt.Errorf("link jail input %s: %w", src, err)
	}
	if writable {
		if err := os.Chown(dst, uid, gid); err != nil {
			return fmt.Errorf("chown jail input %s: %w", dst, err)
		}
	}
	return nil
}

func replaceWithSymlink(target, linkPath string) error {
	if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, linkPath)
}
