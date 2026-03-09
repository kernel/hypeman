package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"al.essio.dev/pkg/shellescape"
	"github.com/kernel/hypeman/lib/vmconfig"
)

// runSystemdMode hands off control to systemd.
// This is used when the image's CMD is /sbin/init or /lib/systemd/systemd.
// The init binary:
// 1. Injects the hypeman-agent.service unit
// 2. Uses chroot to switch to the container rootfs
// 3. Execs the image's entrypoint/cmd (systemd) which becomes the new PID 1
func runSystemdMode(log *Logger, cfg *vmconfig.Config) {
	const newroot = "/overlay/newroot"

	// Inject hypeman-agent.service (skip if guest-agent was not copied)
	// Pass environment variables so they're available via hypeman exec
	if cfg.SkipGuestAgent {
		log.Info("hypeman-init:systemd", "skipping agent service injection (skip_guest_agent=true)")
	} else {
		log.Info("hypeman-init:systemd", "injecting hypeman-agent.service")
		if err := injectAgentService(newroot, cfg.Env); err != nil {
			log.Error("hypeman-init:systemd", "failed to inject service", err)
			// Continue anyway - VM will work, just without agent
		}
	}
	if cfg.SkipKernelHeaders {
		log.Info("hypeman-init:systemd", "skipping kernel headers service injection (skip_kernel_headers=true)")
	} else {
		log.Info("hypeman-init:systemd", "injecting hypeman-kernel-headers.service")
		if err := injectHeadersService(newroot); err != nil {
			log.Error("hypeman-init:systemd", "failed to inject headers service", err)
			_ = writeKernelHeadersStatus(initrdKernelHeadersPaths.statusPath, headersStatusFailed)
			log.Info("hypeman-init:systemd", formatHeadersFailedSentinel(err))
		}
	}

	// Change root to the new filesystem using chroot
	log.Info("hypeman-init:systemd", "executing chroot")
	if err := syscall.Chroot(newroot); err != nil {
		log.Error("hypeman-init:systemd", "chroot failed", err)
		dropToShell()
	}

	// Change to new root directory
	if err := os.Chdir("/"); err != nil {
		log.Error("hypeman-init:systemd", "chdir / failed", err)
		dropToShell()
	}

	// Build effective command from entrypoint + cmd
	argv := append(cfg.Entrypoint, cfg.Cmd...)
	if len(argv) == 0 {
		// Fallback to /sbin/init if no command specified
		argv = []string{"/sbin/init"}
	}

	// Exec systemd - this replaces the current process
	log.Info("hypeman-init:systemd", fmt.Sprintf("exec %v", argv))
	log.Info("hypeman-init:systemd", formatProgramStartSentinel("systemd"))

	// syscall.Exec replaces the current process with the new one
	// Use buildEnv to include user's environment variables from the image/instance config
	err := syscall.Exec(argv[0], argv, buildEnv(cfg.Env))
	if err != nil {
		log.Error("hypeman-init:systemd", fmt.Sprintf("exec %s failed", argv[0]), err)
		dropToShell()
	}
}

// injectAgentService creates the systemd service unit for the hypeman guest-agent.
// It also writes an environment file with the configured env vars so they're
// available to commands run via hypeman exec.
func injectAgentService(newroot string, env map[string]string) error {
	serviceContent := `[Unit]
Description=Hypeman Guest Agent

[Service]
Type=simple
ExecStart=/opt/hypeman/guest-agent
EnvironmentFile=-/etc/hypeman/env
Restart=always
RestartSec=3
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
`

	serviceDir := newroot + "/etc/systemd/system"
	wantsDir := serviceDir + "/multi-user.target.wants"
	hypemanDir := newroot + "/etc/hypeman"

	// Create directories
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(wantsDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(hypemanDir, 0755); err != nil {
		return err
	}

	// Write environment file with configured env vars
	// Format: KEY=VALUE, one per line
	envContent := buildEnvFileContent(env)
	envPath := hypemanDir + "/env"
	if err := os.WriteFile(envPath, []byte(envContent), 0644); err != nil {
		return err
	}

	// Write service file
	servicePath := serviceDir + "/hypeman-agent.service"
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return err
	}

	// Enable the service by creating a symlink in wants directory
	symlinkPath := wantsDir + "/hypeman-agent.service"
	// Use relative path for the symlink
	if err := os.Symlink("../hypeman-agent.service", symlinkPath); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// buildEnvFileContent creates systemd environment file content from env map.
// Includes default PATH and HOME if not already set.
// Values are properly quoted and escaped for systemd's EnvironmentFile format
// using shellescape.Quote() which handles shell-style quoting.
func buildEnvFileContent(env map[string]string) string {
	var content string

	// Add user's environment variables
	for k, v := range env {
		content += fmt.Sprintf("%s=%s\n", k, shellescape.Quote(v))
	}

	// Add defaults only if not already set by user
	if _, ok := env["PATH"]; !ok {
		content += "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n"
	}
	if _, ok := env["HOME"]; !ok {
		content += "HOME=/root\n"
	}

	return content
}

func injectHeadersService(newroot string) error {
	if err := stageKernelHeadersAssetsForSystemd(newroot); err != nil {
		return err
	}
	if err := writeKernelHeadersStatus(initrdKernelHeadersPaths.statusPath, headersStatusPending); err != nil {
		return err
	}

	serviceContent := `[Unit]
Description=Hypeman Kernel Headers Setup
After=local-fs.target
ConditionPathExists=/opt/hypeman/hypeman-init
ConditionPathExists=/opt/hypeman/kernel-headers.tar.gz

[Service]
Type=oneshot
ExecStart=/opt/hypeman/hypeman-init --headers-worker-guest
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
`

	serviceDir := filepath.Join(newroot, "etc/systemd/system")
	wantsDir := filepath.Join(serviceDir, "multi-user.target.wants")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(wantsDir, 0755); err != nil {
		return err
	}

	servicePath := filepath.Join(serviceDir, "hypeman-kernel-headers.service")
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return err
	}

	symlinkPath := filepath.Join(wantsDir, "hypeman-kernel-headers.service")
	if err := os.Symlink("../hypeman-kernel-headers.service", symlinkPath); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func stageKernelHeadersAssetsForSystemd(newroot string) error {
	const (
		dstDir        = "opt/hypeman"
		dstInitBinRel = "opt/hypeman/hypeman-init"
		dstTarballRel = "opt/hypeman/kernel-headers.tar.gz"
		sourceTarball = "/kernel-headers.tar.gz"
	)

	srcInitBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve init binary path: %w", err)
	}

	guestDstDir := filepath.Join(newroot, dstDir)
	if err := os.MkdirAll(guestDstDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", guestDstDir, err)
	}

	initData, err := os.ReadFile(srcInitBin)
	if err != nil {
		return fmt.Errorf("read init binary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(newroot, dstInitBinRel), initData, 0755); err != nil {
		return fmt.Errorf("write init binary: %w", err)
	}

	if _, err := os.Stat(sourceTarball); err != nil {
		return fmt.Errorf("stat headers tarball: %w", err)
	}

	targetTarball := filepath.Join(newroot, dstTarballRel)
	if _, err := os.Stat(targetTarball); os.IsNotExist(err) {
		if err := os.WriteFile(targetTarball, []byte{}, 0644); err != nil {
			return fmt.Errorf("create target tarball file: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat target tarball file: %w", err)
	}

	if err := bindMount(sourceTarball, targetTarball); err != nil {
		return fmt.Errorf("bind mount headers tarball: %w", err)
	}

	return nil
}
