package qemu

import (
	"context"
	"fmt"
	"time"

	"github.com/digitalocean/go-qemu/qemu"
	"github.com/digitalocean/go-qemu/qmp"
	"github.com/digitalocean/go-qemu/qmp/raw"
)

// QMP client timeout constants
const (
	// qmpConnectTimeout is the timeout for connecting to the QMP socket
	qmpConnectTimeout = 1 * time.Second

	// qmpPollInterval is how often to poll status in WaitMigration and WaitVMReady
	qmpPollInterval = 50 * time.Millisecond
)

// Client wraps go-qemu's Domain and raw.Monitor with convenience methods.
type Client struct {
	domain *qemu.Domain
	raw    *raw.Monitor
	mon    *qmp.SocketMonitor
}

// NewClient creates a new QEMU client connected to the given socket.
func NewClient(socketPath string) (*Client, error) {
	mon, err := qmp.NewSocketMonitor("unix", socketPath, qmpConnectTimeout)
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

// WaitMigration polls until an outgoing migration completes or times out.
// Used for snapshot/standby operations where we initiate the migration.
// Returns nil if migration completed successfully, error otherwise.
func (c *Client) WaitMigration(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		info, err := c.QueryMigration()
		if err != nil {
			// Ignore transient errors during migration, keep polling
			time.Sleep(qmpPollInterval)
			continue
		}

		// Check migration status (Status is a pointer in MigrationInfo)
		if info.Status == nil {
			// Status not available yet, continue polling
			time.Sleep(qmpPollInterval)
			continue
		}

		switch *info.Status {
		case raw.MigrationStatusCompleted:
			return nil
		case raw.MigrationStatusFailed:
			if info.ErrorDesc != nil && *info.ErrorDesc != "" {
				return fmt.Errorf("migration failed: %s", *info.ErrorDesc)
			}
			return fmt.Errorf("migration failed")
		case raw.MigrationStatusCancelled:
			return fmt.Errorf("migration cancelled")
		case raw.MigrationStatusNone, raw.MigrationStatusActive, raw.MigrationStatusSetup, raw.MigrationStatusPreSwitchover, raw.MigrationStatusDevice:
			// Still in progress or not started yet, continue polling
		}

		time.Sleep(qmpPollInterval)
	}

	return fmt.Errorf("migration timeout after %v", timeout)
}

// WaitVMReady polls until the VM is ready after an incoming migration.
// Used for restore operations where QEMU was started with -incoming.
// The VM transitions from "inmigrate" to "paused" when migration data is loaded.
// Returns nil when VM is ready for resume, error on timeout or failure.
func (c *Client) WaitVMReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		status, err := c.Status()
		if err != nil {
			// Ignore transient errors, keep polling
			time.Sleep(qmpPollInterval)
			continue
		}

		switch status {
		case qemu.StatusPaused, qemu.StatusPostMigrate:
			// VM has finished loading migration data and is ready for resume
			return nil
		case qemu.StatusInMigrate, qemu.StatusRestoreVM:
			// Still loading migration data, continue polling
		case qemu.StatusRunning:
			// Already running (shouldn't happen, but not an error)
			return nil
		case qemu.StatusGuestPanicked, qemu.StatusInternalError, qemu.StatusIOError:
			return fmt.Errorf("VM in error state: %v", status)
		case qemu.StatusShutdown:
			return fmt.Errorf("VM shut down during migration")
		default:
			// Other states - keep polling
		}

		time.Sleep(qmpPollInterval)
	}

	return fmt.Errorf("timeout waiting for VM ready after %v", timeout)
}
