//go:build darwin

package vz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Client implements hypervisor.Hypervisor via HTTP to the vz-shim process.
type Client struct {
	socketPath            string
	httpClient            *http.Client
	longRunningHTTPClient *http.Client
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
	longRunningHTTPClient := &http.Client{
		Transport: transport,
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
		socketPath:            socketPath,
		httpClient:            httpClient,
		longRunningHTTPClient: longRunningHTTPClient,
	}, nil
}

var _ hypervisor.Hypervisor = (*Client)(nil)

// vmInfoResponse matches the shim's VMInfoResponse structure.
type vmInfoResponse struct {
	State string `json:"state"`
}

type snapshotRequest struct {
	DestinationPath string `json:"destination_path"`
}

type balloonResponse struct {
	TargetGuestMemoryBytes int64 `json:"target_guest_memory_bytes"`
}

func (c *Client) Capabilities() hypervisor.Capabilities {
	return capabilities()
}

func capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot:            runtime.GOARCH == "arm64",
		SupportsHotplugMemory:       false,
		SupportsBalloonControl:      true,
		SupportsPause:               true,
		SupportsVsock:               true,
		SupportsGPUPassthrough:      false,
		SupportsDiskIOLimit:         false,
		SupportsGracefulVMMShutdown: true,
		SupportsSnapshotBaseReuse:   false,
	}
}

// doPut sends a PUT request to the shim and checks for success.
func (c *Client) doPut(ctx context.Context, path string, body io.Reader) error {
	return c.doPutWithClient(ctx, c.httpClient, path, body)
}

func (c *Client) doPutWithClient(ctx context.Context, client *http.Client, path string, body io.Reader) error {
	attrs := hypervisor.TraceAttributesFromContext(ctx)
	attrs = append(attrs,
		attribute.String("operation", http.MethodPut+" "+path),
		attribute.String("http.method", http.MethodPut),
		attribute.String("http.route", path),
	)
	ctx, span := otel.Tracer("hypeman/hypervisor/vz").Start(ctx, "hypervisor.http PUT "+path, trace.WithAttributes(attrs...))
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://vz-shim"+path, body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		span.SetStatus(codes.Error, resp.Status)
		return fmt.Errorf("%s failed with status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// doGet sends a GET request to the shim and returns the response body.
func (c *Client) doGet(ctx context.Context, path string) ([]byte, error) {
	attrs := hypervisor.TraceAttributesFromContext(ctx)
	attrs = append(attrs,
		attribute.String("operation", http.MethodGet+" "+path),
		attribute.String("http.method", http.MethodGet),
		attribute.String("http.route", path),
	)
	tracer := otel.Tracer("hypeman/hypervisor/vz")
	spanName := "hypervisor.http GET " + path
	shouldTrace := hypervisor.ShouldTraceHypervisorHTTPSpan(http.MethodGet, path)

	var span trace.Span
	if shouldTrace {
		var spanCtx context.Context
		spanCtx, span = tracer.Start(ctx, spanName, trace.WithAttributes(attrs...))
		ctx = spanCtx
		defer span.End()
	}

	recordError := func(err error) {
		if shouldTrace {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vz-shim"+path, nil)
	if err != nil {
		recordError(err)
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		recordError(err)
		return nil, err
	}
	defer resp.Body.Close()
	if shouldTrace {
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		recordError(err)
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if shouldTrace {
			span.SetStatus(codes.Error, resp.Status)
		}
	} else {
		if shouldTrace {
			span.SetStatus(codes.Ok, "")
		}
	}
	return body, nil
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
	case "Running", "Resuming":
		state = hypervisor.StateRunning
	case "Paused", "Pausing", "Saving":
		state = hypervisor.StatePaused
	case "Starting", "Restoring":
		state = hypervisor.StateCreated
	case "Shutdown", "Stopped", "Stopping", "Error":
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

func (c *Client) Snapshot(ctx context.Context, destPath string, _ hypervisor.SnapshotOptions) error {
	req := snapshotRequest{DestinationPath: destPath}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal snapshot request: %w", err)
	}
	// Snapshot duration scales with guest RAM size, so rely on caller context
	// rather than the default short client timeout.
	return c.doPutWithClient(ctx, c.longRunningHTTPClient, "/api/v1/vm.snapshot", bytes.NewReader(body))
}

func (c *Client) ResizeMemory(ctx context.Context, bytes int64) error {
	return hypervisor.ErrNotSupported
}

func (c *Client) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	return hypervisor.ErrNotSupported
}

func (c *Client) SetTargetGuestMemoryBytes(ctx context.Context, targetBytes int64) error {
	reqBody, err := json.Marshal(balloonResponse{TargetGuestMemoryBytes: targetBytes})
	if err != nil {
		return fmt.Errorf("marshal balloon target: %w", err)
	}
	return c.doPut(ctx, "/api/v1/vm.balloon", bytes.NewReader(reqBody))
}

func (c *Client) GetTargetGuestMemoryBytes(ctx context.Context) (int64, error) {
	body, err := c.doGet(ctx, "/api/v1/vm.balloon")
	if err != nil {
		return 0, fmt.Errorf("get balloon target: %w", err)
	}

	var resp balloonResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode balloon target: %w", err)
	}
	return resp.TargetGuestMemoryBytes, nil
}
