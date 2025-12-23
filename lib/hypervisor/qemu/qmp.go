package qemu

import (
	"context"
	"fmt"
	"time"

	"github.com/digitalocean/go-qemu/qemu"
	"github.com/digitalocean/go-qemu/qmp"
	"github.com/digitalocean/go-qemu/qmp/raw"
)

// Client wraps go-qemu's Domain and raw.Monitor with convenience methods.
type Client struct {
	domain *qemu.Domain
	raw    *raw.Monitor
	mon    *qmp.SocketMonitor
}

// NewClient creates a new QEMU client connected to the given socket.
func NewClient(socketPath string) (*Client, error) {
	mon, err := qmp.NewSocketMonitor("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("create socket monitor: %w", err)
	}

	if err := mon.Connect(); err != nil {
		return nil, fmt.Errorf("connect to qmp: %w", err)
	}

	domain, err := qemu.NewDomain(mon, "vm")
	if err != nil {
		mon.Disconnect()
		return nil, fmt.Errorf("create domain: %w", err)
	}

	return &Client{
		domain: domain,
		raw:    raw.NewMonitor(mon),
		mon:    mon,
	}, nil
}

// Close disconnects from the QMP socket.
func (c *Client) Close() error {
	return c.domain.Close()
}

// Stop pauses VM execution (QMP 'stop' command).
func (c *Client) Stop() error {
	return c.raw.Stop()
}

// Continue resumes VM execution (QMP 'cont' command).
func (c *Client) Continue() error {
	return c.raw.Cont()
}

// Status returns the current VM status as a typed enum.
func (c *Client) Status() (qemu.Status, error) {
	return c.domain.Status()
}

// StatusInfo returns detailed status information from the raw monitor.
func (c *Client) StatusInfo() (raw.StatusInfo, error) {
	return c.raw.QueryStatus()
}

// Quit shuts down QEMU (QMP 'quit' command).
func (c *Client) Quit() error {
	return c.raw.Quit()
}

// SystemPowerdown sends ACPI power button event (graceful shutdown).
func (c *Client) SystemPowerdown() error {
	return c.raw.SystemPowerdown()
}

// SystemReset resets the VM (hard reset).
func (c *Client) SystemReset() error {
	return c.raw.SystemReset()
}

// Version returns the QEMU version string.
func (c *Client) Version() (string, error) {
	return c.domain.Version()
}

// Events returns a channel for receiving QEMU events.
func (c *Client) Events() (chan qmp.Event, chan struct{}, error) {
	return c.domain.Events()
}

// Run executes a raw QMP command (for commands not yet wrapped).
func (c *Client) Run(cmd qmp.Command) ([]byte, error) {
	return c.domain.Run(cmd)
}

// Migrate initiates a migration to the given URI (typically "file:///path").
// This is used for saving VM state to a file for snapshot/standby.
func (c *Client) Migrate(uri string) error {
	// Migrate(uri, blk, inc, detach) - we use nil for optional params
	return c.raw.Migrate(uri, nil, nil, nil)
}

// QueryMigration returns the current migration status.
func (c *Client) QueryMigration() (raw.MigrationInfo, error) {
	return c.raw.QueryMigrate()
}

// WaitMigration polls until migration completes or times out.
// Returns nil if migration completed successfully, error otherwise.
func (c *Client) WaitMigration(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pollInterval := 50 * time.Millisecond

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		info, err := c.QueryMigration()
		if err != nil {
			return fmt.Errorf("query migration: %w", err)
		}

		// Check migration status (Status is a pointer in MigrationInfo)
		if info.Status == nil {
			// Status not available yet, continue polling
			time.Sleep(pollInterval)
			continue
		}

		switch *info.Status {
		case raw.MigrationStatusCompleted:
			return nil
		case raw.MigrationStatusFailed:
			return fmt.Errorf("migration failed")
		case raw.MigrationStatusCancelled:
			return fmt.Errorf("migration cancelled")
		case raw.MigrationStatusActive, raw.MigrationStatusSetup, raw.MigrationStatusPreSwitchover, raw.MigrationStatusDevice:
			// Still in progress, continue polling
		default:
			// Unknown or "none" status - might not have started yet
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("migration timeout after %v", timeout)
}
