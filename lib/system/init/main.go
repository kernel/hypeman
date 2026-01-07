// Package main implements the hypeman init binary that runs as PID 1 in guest VMs.
//
// This binary replaces the shell-based init script with a Go program that provides:
// - Human-readable structured logging
// - Clean separation of boot phases
// - Support for both exec mode (container-like) and systemd mode (full VM)
//
// Note: This binary is called by init.sh wrapper which mounts /proc, /sys, /dev
// before the Go runtime starts (Go requires these during initialization).
package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	log := NewLogger()
	log.Info("boot", "init starting")

	// Phase 1: Mount additional filesystems (proc/sys/dev already mounted by init.sh)
	if err := mountEssentials(log); err != nil {
		log.Error("mount", "failed to mount essentials", err)
		dropToShell()
	}

	// Phase 2: Setup overlay rootfs
	if err := setupOverlay(log); err != nil {
		log.Error("overlay", "failed to setup overlay", err)
		dropToShell()
	}

	// Phase 3: Read and parse config
	cfg, err := readConfig(log)
	if err != nil {
		log.Error("config", "failed to read config", err)
		dropToShell()
	}

	// Phase 4: Configure network (shared between modes)
	if cfg.NetworkEnabled {
		if err := configureNetwork(log, cfg); err != nil {
			log.Error("network", "failed to configure network", err)
			// Continue anyway - network isn't always required
		}
	}

	// Phase 5: Load GPU drivers if needed
	if cfg.HasGPU {
		if err := loadGPUDrivers(log); err != nil {
			log.Error("gpu", "failed to load GPU drivers", err)
			// Continue anyway
		}
	}

	// Phase 6: Mount volumes
	if len(cfg.VolumeMounts) > 0 {
		if err := mountVolumes(log, cfg); err != nil {
			log.Error("volumes", "failed to mount volumes", err)
			// Continue anyway
		}
	}

	// Phase 7: Bind mount filesystems to new root
	if err := bindMountsToNewRoot(log); err != nil {
		log.Error("bind", "failed to bind mounts", err)
		dropToShell()
	}

	// Phase 8: Copy guest-agent to target location
	if err := copyGuestAgent(log); err != nil {
		log.Error("agent", "failed to copy guest-agent", err)
		// Continue anyway - exec will still work, just no remote access
	}

	// Phase 9: Mode-specific execution
	if cfg.InitMode == "systemd" {
		log.Info("mode", "entering systemd mode")
		runSystemdMode(log, cfg)
	} else {
		log.Info("mode", "entering exec mode")
		runExecMode(log, cfg)
	}
}

// dropToShell drops to an interactive shell for debugging when boot fails
func dropToShell() {
	fmt.Fprintln(os.Stderr, "FATAL: dropping to shell for debugging")
	cmd := exec.Command("/bin/sh", "-i")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
	os.Exit(1)
}

