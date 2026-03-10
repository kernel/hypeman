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
	"path/filepath"
	"strings"
	"sync"

	"github.com/kernel/hypeman/lib/vmconfig"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case headersWorkerArg:
			runKernelHeadersWorker(NewLogger(), initrdKernelHeadersPaths)
			return
		case headersWorkerGuestArg:
			runKernelHeadersWorker(NewLogger(), guestKernelHeadersPaths)
			return
		}
	}

	log := NewLogger()
	log.Info("hypeman-init:boot", "init starting")

	// Phase 1: Mount additional filesystems (proc/sys/dev already mounted by init.sh)
	if err := mountEssentials(log); err != nil {
		log.Error("hypeman-init:mount", "failed to mount essentials", err)
		dropToShell()
	}

	// Phase 2: Setup overlay rootfs
	if err := setupOverlay(log); err != nil {
		log.Error("hypeman-init:overlay", "failed to setup overlay", err)
		dropToShell()
	}

	// Phase 3: Read and parse config
	cfg, err := readConfig(log)
	if err != nil {
		log.Error("hypeman-init:config", "failed to read config", err)
		dropToShell()
	}

	runNetworkSetup := func() {
		if !cfg.NetworkEnabled {
			return
		}
		if err := configureNetwork(log, cfg); err != nil {
			log.Error("hypeman-init:network", "failed to configure network", err)
			// Continue anyway - network isn't always required
		}
	}
	runVolumesSetup := func() {
		if len(cfg.VolumeMounts) == 0 {
			return
		}
		if err := mountVolumes(log, cfg); err != nil {
			log.Error("hypeman-init:volumes", "failed to mount volumes", err)
			// Continue anyway
		}
	}

	// Phase 4/5: Run setup tasks.
	// Network + volume setup are parallelized only when mounted paths are disjoint
	// from /etc, because network setup writes /overlay/newroot/etc/resolv.conf.
	if shouldRunNetworkAndVolumesInParallel(cfg) {
		var setupWG sync.WaitGroup
		setupWG.Add(2)
		go func() {
			defer setupWG.Done()
			runNetworkSetup()
		}()
		go func() {
			defer setupWG.Done()
			runVolumesSetup()
		}()
		setupWG.Wait()
	} else {
		// When /etc (or /etc/*) is volume-mounted, configure network after volumes
		// so resolv.conf is written into the mounted path instead of being hidden.
		runVolumesSetup()
		runNetworkSetup()
	}

	// Phase 6: Bind mount filesystems to new root
	if err := bindMountsToNewRoot(log); err != nil {
		log.Error("hypeman-init:bind", "failed to bind mounts", err)
		dropToShell()
	}

	// Phase 7: Copy guest-agent to target location (skips if already exists or skip_guest_agent=true)
	if err := copyGuestAgent(log, cfg.SkipGuestAgent); err != nil {
		log.Error("hypeman-init:agent", "failed to copy guest-agent", err)
		// Continue anyway - exec will still work, just no remote access
	}

	// Phase 8: Start async kernel headers setup for exec mode.
	// In systemd mode, service injection is handled during runSystemdMode.
	if cfg.SkipKernelHeaders {
		log.Info("hypeman-init:headers", "skipping kernel headers setup (skip_kernel_headers=true)")
	} else if cfg.InitMode == "exec" {
		startKernelHeadersWorkerAsync(log)
	}

	// Phase 9: Mode-specific execution
	if cfg.InitMode == "systemd" {
		log.Info("hypeman-init:mode", "entering systemd mode")
		runSystemdMode(log, cfg)
	} else {
		log.Info("hypeman-init:mode", "entering exec mode")
		runExecMode(log, cfg)
	}
}

func shouldRunNetworkAndVolumesInParallel(cfg *vmconfig.Config) bool {
	if !cfg.NetworkEnabled || len(cfg.VolumeMounts) == 0 {
		return false
	}

	for _, vol := range cfg.VolumeMounts {
		// Normalize to an absolute path inside the guest.
		mountPath := filepath.Clean("/" + strings.TrimPrefix(vol.Path, "/"))
		if mountPath == "/" || mountPath == "/etc" || strings.HasPrefix(mountPath, "/etc/") {
			return false
		}
	}

	return true
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
