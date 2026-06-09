package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

const (
	// newRoot is where the overlay rootfs is mounted; both exec and systemd mode
	// chroot into it before the workload runs.
	newRoot = "/overlay/newroot"

	// rosettaShareDir holds the Rosetta interpreter and lives inside the overlay
	// rootfs (under /opt rather than /run) so it stays reachable at the same
	// guest-absolute path after the chroot and is not shadowed by a /run tmpfs
	// that systemd remounts during boot.
	rosettaShareDir = "/opt/hypeman/rosetta"

	// rosettaInterp is the interpreter path as seen from inside the chroot
	// (guest-absolute). It is what the binfmt rule and the systemd binfmt.d
	// drop-in reference.
	rosettaInterp = rosettaShareDir + "/rosetta"

	binfmtMountPoint = "/proc/sys/fs/binfmt_misc"
	binfmtRegister   = binfmtMountPoint + "/register"

	// systemd reads binfmt.d drop-ins from /usr/lib/binfmt.d at boot.
	binfmtdDir  = "/usr/lib/binfmt.d"
	binfmtdConf = binfmtdDir + "/rosetta.conf"

	// x86-64 ELF magic/mask, matching the rosetta binfmt rule shipped by systemd.
	// magic: ELF, little-endian, 64-bit, EM_X86_64 (0x3e) in e_machine.
	rosettaELFMagic = `\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00`
	rosettaELFMask  = `\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff`
)

// rosettaBinfmtRule builds the binfmt_misc registration string for x86-64 ELF
// binaries dispatched through the Rosetta interpreter at interp. The flags are
// OCF: O preserves argv[0], C applies the target's credentials, and F opens the
// interpreter at registration and pins the fd so it survives the later chroot
// into the overlay rootfs.
func rosettaBinfmtRule(interp string) string {
	return fmt.Sprintf(":rosetta:M::%s:%s:%s:OCF", rosettaELFMagic, rosettaELFMask, interp)
}

// setupRosetta mounts the Rosetta virtio-fs share into the overlay rootfs and
// registers a binfmt_misc handler so x86-64 ELF binaries execute via Rosetta.
// In systemd mode it also drops a binfmt.d config so systemd-binfmt re-registers
// the handler if it flushes binfmt_misc while applying the image's own rules.
func setupRosetta(log *Logger, systemdMode bool) error {
	hostShareDir := filepath.Join(newRoot, rosettaShareDir)
	if err := os.MkdirAll(hostShareDir, 0o755); err != nil {
		return fmt.Errorf("mkdir rosetta share: %w", err)
	}
	if err := mount(shimconfig.RosettaMountTag, hostShareDir, "virtiofs", ""); err != nil {
		return fmt.Errorf("mount rosetta share: %w", err)
	}

	hostInterp := filepath.Join(newRoot, rosettaInterp)
	if err := waitForDevice(hostInterp, 2*time.Second); err != nil {
		return fmt.Errorf("rosetta interpreter not present: %w", err)
	}

	if err := ensureBinfmtMounted(); err != nil {
		return fmt.Errorf("mount binfmt_misc: %w", err)
	}

	// The F flag opens the interpreter at registration time, relative to init's
	// root, so the live rule must name the interpreter by its current path under
	// newRoot; the pinned fd then survives the chroot.
	rule := rosettaBinfmtRule(hostInterp)
	existed, err := registerBinfmt(rule)
	if err != nil {
		return err
	}
	if existed {
		log.Info("hypeman-init:rosetta", "existing :rosetta: binfmt_misc handler found; leaving it in place")
	}

	if systemdMode {
		if err := writeBinfmtdConf(log); err != nil {
			return err
		}
	}

	log.Info("hypeman-init:rosetta", "registered rosetta binfmt_misc handler")
	return nil
}

// registerBinfmt writes a binfmt_misc rule, tolerating an already-registered
// :rosetta: handler (e.g. a restored boot or an image that ships its own rule):
// the kernel returns EEXIST for a duplicate name, which is benign. It reports
// whether such an existing handler was found so the caller can log it.
func registerBinfmt(rule string) (existed bool, err error) {
	werr := os.WriteFile(binfmtRegister, []byte(rule), 0o644)
	if alreadyRegistered(werr) {
		return true, nil
	}
	if werr != nil {
		return false, fmt.Errorf("register rosetta binfmt: %w", werr)
	}
	return false, nil
}

// alreadyRegistered reports whether a binfmt_misc register write failed only
// because a handler of that name already exists (EEXIST).
func alreadyRegistered(err error) bool {
	return errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.EEXIST)
}

// writeBinfmtdConf drops a systemd binfmt.d config that re-registers Rosetta
// against its guest-absolute path inside the chroot. systemd-binfmt flushes all
// binfmt_misc rules before applying binfmt.d, so without this the live rule
// registered above is lost whenever the image ships its own binfmt.d entries.
func writeBinfmtdConf(log *Logger) error {
	dir := filepath.Join(newRoot, binfmtdDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir binfmt.d: %w", err)
	}
	rule := rosettaBinfmtRule(rosettaInterp)
	if err := os.WriteFile(filepath.Join(newRoot, binfmtdConf), []byte(rule+"\n"), 0o644); err != nil {
		return fmt.Errorf("write binfmt.d config: %w", err)
	}
	log.Info("hypeman-init:rosetta", "wrote rosetta binfmt.d config for systemd")
	return nil
}

// ensureBinfmtMounted mounts the binfmt_misc filesystem unless it is already
// available.
func ensureBinfmtMounted() error {
	if _, err := os.Stat(binfmtRegister); err == nil {
		return nil
	}
	return syscall.Mount("binfmt_misc", binfmtMountPoint, "binfmt_misc", 0, "")
}
