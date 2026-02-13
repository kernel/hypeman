//go:build darwin

package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		Timeout:   10 * time.Second,
	}

	// Verify connectivity with a short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vz-shim/api/v1/vmm.ping", nil)
	if err != nil {
		return nil, fmt.Errorf("ping shim: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ping shim: %w", err)
	}
	resp.Body.Close()

	return &Client{
		socketPath: socketPath,
		httpClient: httpClient,
	}, nil
}

var _ hypervisor.Hypervisor = (*Client)(nil)

// vmInfoResponse matches the shim's VMInfoResponse structure.
type vmInfoResponse struct {
	State string `json:"state"`
}

func (c *Client) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot:       false,
		SupportsHotplugMemory:  false,
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: false,
		SupportsDiskIOLimit:    false,
	}
}

// doPut sends a PUT request to the shim and checks for success.
func (c *Client) doPut(ctx context.Context, path string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://vz-shim"+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s failed with status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// doGet sends a GET request to the shim and returns the response body.
func (c *Client) doGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vz-shim"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *Client) DeleteVM(ctx context.Context) error {
	return c.doPut(ctx, "/api/v1/vm.shutdown", nil)
}

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

func (c *Client) GetVMInfo(ctx context.Context) (*hypervisor.VMInfo, error) {
	body, err := c.doGet(ctx, "/api/v1/vm.info")
	if err != nil {
		return nil, fmt.Errorf("get vm info: %w", err)
	}

	var info vmInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode vm info: %w", err)
	}

	var state hypervisor.VMState
	switch info.State {
	case "Running":
		state = hypervisor.StateRunning
	case "Paused":
		state = hypervisor.StatePaused
	case "Starting":
		state = hypervisor.StateCreated
	case "Shutdown", "Stopped", "Error":
		state = hypervisor.StateShutdown
	default:
		state = hypervisor.StateShutdown
	}

	return &hypervisor.VMInfo{State: state}, nil
}

func (c *Client) Pause(ctx context.Context) error {
	return c.doPut(ctx, "/api/v1/vm.pause", nil)
}

func (c *Client) Resume(ctx context.Context) error {
	return c.doPut(ctx, "/api/v1/vm.resume", nil)
}

func (c *Client) Snapshot(ctx context.Context, destPath string) error {
	return hypervisor.ErrNotSupported
}

func (c *Client) ResizeMemory(ctx context.Context, bytes int64) error {
	return hypervisor.ErrNotSupported
}

func (c *Client) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	return hypervisor.ErrNotSupported
}
