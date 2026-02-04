//go:build darwin

package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// Client implements hypervisor.Hypervisor via HTTP to the vz-shim process.
type Client struct {
	socketPath string
	httpClient *http.Client
}

// NewClient creates a new vz shim client.
func NewClient(socketPath string) (*Client, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	// Verify connectivity
	resp, err := httpClient.Get("http://vz-shim/api/v1/vmm.ping")
	if err != nil {
		return nil, fmt.Errorf("ping shim: %w", err)
	}
	resp.Body.Close()

	return &Client{
		socketPath: socketPath,
		httpClient: httpClient,
	}, nil
}

// Verify Client implements the interface
var _ hypervisor.Hypervisor = (*Client)(nil)

// vmInfoResponse matches the shim's VMInfoResponse structure.
type vmInfoResponse struct {
	State string `json:"state"`
}

// Capabilities returns the features supported by vz.
func (c *Client) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot:       false, // Not implemented via shim yet
		SupportsHotplugMemory:  false,
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: false,
		SupportsDiskIOLimit:    false,
	}
}

// DeleteVM requests a graceful shutdown of the guest.
func (c *Client) DeleteVM(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://vz-shim/api/v1/vm.shutdown", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shutdown failed with status %d", resp.StatusCode)
	}

	return nil
}

// Shutdown stops the VMM (shim) forcefully.
func (c *Client) Shutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://vz-shim/api/v1/vmm.shutdown", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Connection reset is expected when shim exits
		return nil
	}
	defer resp.Body.Close()

	return nil
}

// GetVMInfo returns current VM state information.
func (c *Client) GetVMInfo(ctx context.Context) (*hypervisor.VMInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vz-shim/api/v1/vm.info", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get vm info: %w", err)
	}
	defer resp.Body.Close()

	var info vmInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode vm info: %w", err)
	}

	var state hypervisor.VMState
	switch info.State {
	case "Running":
		state = hypervisor.StateRunning
	case "Paused":
		state = hypervisor.StatePaused
	case "Shutdown", "Stopped":
		state = hypervisor.StateShutdown
	default:
		state = hypervisor.StateRunning
	}

	return &hypervisor.VMInfo{State: state}, nil
}

// Pause suspends VM execution.
func (c *Client) Pause(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://vz-shim/api/v1/vm.pause", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pause request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pause failed with status %d", resp.StatusCode)
	}

	return nil
}

// Resume continues VM execution after pause.
func (c *Client) Resume(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://vz-shim/api/v1/vm.resume", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("resume request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resume failed with status %d", resp.StatusCode)
	}

	return nil
}

// Snapshot is not supported via shim yet.
func (c *Client) Snapshot(ctx context.Context, destPath string) error {
	return fmt.Errorf("snapshot not implemented via shim")
}

// ResizeMemory is not supported by vz.
func (c *Client) ResizeMemory(ctx context.Context, bytes int64) error {
	return fmt.Errorf("memory resize not supported by vz")
}

// ResizeMemoryAndWait is not supported by vz.
func (c *Client) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	return fmt.Errorf("memory resize not supported by vz")
}
