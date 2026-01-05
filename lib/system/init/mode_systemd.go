package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/onkernel/hypeman/lib/vmconfig"
)

// runSystemdMode hands off control to systemd.
// This is used when the image's CMD is /sbin/init or /lib/systemd/systemd.
// The init binary:
// 1. Injects the hypeman-agent.service unit
// 2. Uses chroot to switch to the container rootfs
// 3. Execs the image's entrypoint/cmd (systemd) which becomes the new PID 1
func runSystemdMode(log *Logger, cfg *vmconfig.Config) {
	const newroot = "/overlay/newroot"

	// Inject hypeman-agent.service
	log.Info("systemd", "injecting hypeman-agent.service")
	if err := injectAgentService(newroot); err != nil {
		log.Error("systemd", "failed to inject service", err)
		// Continue anyway - VM will work, just without agent
	}

	// Change root to the new filesystem using chroot
	log.Info("systemd", "executing chroot")
	if err := syscall.Chroot(newroot); err != nil {
		log.Error("systemd", "chroot failed", err)
		dropToShell()
	}

	// Change to new root directory
	if err := os.Chdir("/"); err != nil {
		log.Error("systemd", "chdir / failed", err)
		dropToShell()
	}

	// Build effective command from entrypoint + cmd
	argv := append(cfg.Entrypoint, cfg.Cmd...)
	if len(argv) == 0 {
		// Fallback to /sbin/init if no command specified
		argv = []string{"/sbin/init"}
	}

	// Exec systemd - this replaces the current process
	log.Info("systemd", fmt.Sprintf("exec %v", argv))

	// syscall.Exec replaces the current process with the new one
	// Use buildEnv to include user's environment variables from the image/instance config
	err := syscall.Exec(argv[0], argv, buildEnv(cfg.Env))
	if err != nil {
		log.Error("systemd", fmt.Sprintf("exec %s failed", argv[0]), err)
		dropToShell()
	}
}

// injectAgentService creates the systemd service unit for the hypeman guest-agent.
func injectAgentService(newroot string) error {
	serviceContent := `[Unit]
Description=Hypeman Guest Agent
After=network.target
Wants=network.target

[Service]
Type=simple
ExecStart=/opt/hypeman/guest-agent
Restart=always
RestartSec=3
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

	serviceDir := newroot + "/etc/systemd/system"
	wantsDir := serviceDir + "/multi-user.target.wants"

	// Create directories
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(wantsDir, 0755); err != nil {
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
	return os.Symlink("../hypeman-agent.service", symlinkPath)
}

