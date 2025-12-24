package main

import (
	"fmt"
	"os"
	"os/exec"
)

// configureNetwork sets up networking in the guest VM.
// This is done from the initrd before pivot_root so it works for both exec and systemd modes.
func configureNetwork(log *Logger, cfg *Config) error {
	// Bring up loopback interface
	if err := runIP("link", "set", "lo", "up"); err != nil {
		return fmt.Errorf("bring up lo: %w", err)
	}

	// Add IP address to eth0
	addr := fmt.Sprintf("%s/%s", cfg.GuestIP, cfg.GuestCIDR)
	if err := runIP("addr", "add", addr, "dev", "eth0"); err != nil {
		return fmt.Errorf("add IP address: %w", err)
	}

	// Bring up eth0
	if err := runIP("link", "set", "eth0", "up"); err != nil {
		return fmt.Errorf("bring up eth0: %w", err)
	}

	// Add default route
	if err := runIP("route", "add", "default", "via", cfg.GuestGW); err != nil {
		return fmt.Errorf("add default route: %w", err)
	}

	// Configure DNS in the new root
	resolvConf := fmt.Sprintf("nameserver %s\n", cfg.GuestDNS)
	resolvPath := "/overlay/newroot/etc/resolv.conf"

	// Ensure /etc exists
	if err := os.MkdirAll("/overlay/newroot/etc", 0755); err != nil {
		return fmt.Errorf("mkdir /etc: %w", err)
	}

	if err := os.WriteFile(resolvPath, []byte(resolvConf), 0644); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}

	log.Info("network", fmt.Sprintf("configured eth0 with %s", addr))
	return nil
}

// runIP executes an 'ip' command with the given arguments.
func runIP(args ...string) error {
	cmd := exec.Command("/sbin/ip", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, output)
	}
	return nil
}
