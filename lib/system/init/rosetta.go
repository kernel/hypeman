package main

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

const (
	rosettaMountPoint = "/run/rosetta"
	rosettaInterp     = rosettaMountPoint + "/rosetta"
	binfmtMountPoint  = "/proc/sys/fs/binfmt_misc"
	binfmtRegister    = binfmtMountPoint + "/register"

	// x86-64 ELF magic/mask, matching the rosetta binfmt rule shipped by systemd.
	// magic: ELF, little-endian, 64-bit, EM_X86_64 (0x3e) in e_machine.
	rosettaELFMagic = `\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00`
	rosettaELFMask  = `\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff`
)

// rosettaBinfmtRule builds the binfmt_misc registration string for x86-64 ELF
// binaries dispatched through the Rosetta interpreter at interp. The flags are
// OCF: O preserves argv[0], C applies the target's credentials, and F opens the
// interpreter at registration and pins the fd so it survives the later chroot
// into /overlay/newroot.
func rosettaBinfmtRule(interp string) string {
	return fmt.Sprintf(":rosetta:M::%s:%s:%s:OCF", rosettaELFMagic, rosettaELFMask, interp)
}

// setupRosetta mounts the Rosetta virtio-fs share and registers a binfmt_misc
// handler so x86-64 ELF binaries execute via Rosetta.
func setupRosetta(log *Logger) error {
	if err := os.MkdirAll(rosettaMountPoint, 0o755); err != nil {
		return fmt.Errorf("mkdir rosetta mount: %w", err)
	}
	if err := mount(shimconfig.RosettaMountTag, rosettaMountPoint, "virtiofs", ""); err != nil {
		return fmt.Errorf("mount rosetta share: %w", err)
	}

	if err := waitForDevice(rosettaInterp, 2*time.Second); err != nil {
		return fmt.Errorf("rosetta interpreter not present: %w", err)
	}

	if err := ensureBinfmtMounted(); err != nil {
		return fmt.Errorf("mount binfmt_misc: %w", err)
	}

	if err := os.WriteFile(binfmtRegister, []byte(rosettaBinfmtRule(rosettaInterp)), 0o644); err != nil {
		return fmt.Errorf("register rosetta binfmt: %w", err)
	}

	log.Info("hypeman-init:rosetta", "registered rosetta binfmt_misc handler")
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
