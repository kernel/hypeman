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
// It is entered after the root switch, so all paths are image paths. The
// init binary:
// 1. Injects the hypeman-agent.service and kernel-headers units
// 2. Execs the image's entrypoint/cmd (systemd) which becomes the new PID 1
func runSystemdMode(log *Logger, cfg *vmconfig.Config) {
	// Inject hypeman-agent.service (skip if guest-agent was not copied)
	// Pass environment variables so they're available via hypeman exec
	if cfg.SkipGuestAgent {
		log.Info("hypeman-init:systemd", "skipping agent service injection (skip_guest_agent=true)")
	} else {
		log.Info("hypeman-init:systemd", "injecting hypeman-agent.service")
		if err := injectAgentService("/", cfg.Env); err != nil {
			log.Error("hypeman-init:systemd", "failed to inject service", err)
			// Continue anyway - VM will work, just without agent
		}
	}
	if cfg.SkipKernelHeaders {
		log.Info("hypeman-init:systemd", "skipping kernel headers service injection (skip_kernel_headers=true)")
	} else {
		log.Info("hypeman-init:systemd", "injecting hypeman-kernel-headers.service")
		if err := injectHeadersService("/"); err != nil {
			log.Error("hypeman-init:systemd", "failed to inject headers service", err)
			_ = writeKernelHeadersStatus(headersPaths.statusPath, headersStatusFailed)
			log.Info("hypeman-init:systemd", formatHeadersFailedSentinel(err))
		}
	}

	if err := installEgressProxyCA(log, cfg); err != nil {
		log.Error("hypeman-init:egress-proxy", "egress proxy CA certificate setup failed", err)
		log.Info("hypeman-init:systemd", formatExitSentinel(78, fmt.Sprintf("egress proxy CA certificate setup failed: %v", err)))
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
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

// injectAgentService creates the systemd service unit for the hypeman guest-agent
// under root. It also writes an environment file with the configured env vars
// so they're available to commands run via hypeman exec.
func injectAgentService(root string, env map[string]string) error {
	serviceContent := `[Unit]
Description=Hypeman Guest Agent

[Service]
Type=simple
ExecStart=/opt/hypeman/guest-agent
EnvironmentFile=-/etc/hypeman/env
Restart=always
RestartSec=3
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

	serviceDir := filepath.Join(root, "etc/systemd/system")
	wantsDir := filepath.Join(serviceDir, "multi-user.target.wants")
	hypemanDir := filepath.Join(root, "etc/hypeman")

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
	envPath := filepath.Join(hypemanDir, "env")
	if err := os.WriteFile(envPath, []byte(envContent), 0644); err != nil {
		return err
	}

	// Write service file
	servicePath := filepath.Join(serviceDir, "hypeman-agent.service")
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return err
	}

	// Enable the service by creating a symlink in wants directory
	symlinkPath := filepath.Join(wantsDir, "hypeman-agent.service")
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

// injectHeadersService creates the oneshot unit that installs kernel headers
// under root. The tarball and init binary it runs are staged by
// stageKernelHeadersAssets before the root switch.
func injectHeadersService(root string) error {
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
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

	serviceDir := filepath.Join(root, "etc/systemd/system")
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
