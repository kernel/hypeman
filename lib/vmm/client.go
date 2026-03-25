package vmm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const cloudHypervisorSocketReadyTimeout = 10 * time.Second

const (
	cloudHypervisorStartRetryDelay    = 100 * time.Millisecond
	cloudHypervisorStartRetryAttempts = 20
)

// VMM wraps the generated Cloud Hypervisor client (API v0.3.0)
type VMM struct {
	*ClientWithResponses
	socketPath string
}

// metricsRoundTripper wraps an http.RoundTripper to record metrics
type metricsRoundTripper struct {
	base   http.RoundTripper
	tracer trace.Tracer
}

func (m *metricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	attrs := hypervisor.TraceAttributesFromContext(req.Context())
	attrs = append(attrs,
		attribute.String("operation", req.Method+" "+req.URL.Path),
		attribute.String("http.method", req.Method),
		attribute.String("http.route", req.URL.Path),
	)
	ctx, span := m.tracer.Start(req.Context(), "hypervisor.http "+req.Method+" "+req.URL.Path, trace.WithAttributes(attrs...))
	req = req.WithContext(ctx)
	resp, err := m.base.RoundTrip(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		if resp.StatusCode >= 400 {
			span.SetStatus(codes.Error, resp.Status)
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}
	span.End()

	// Record metrics using global VMMMetrics
	if VMMMetrics != nil {
		operation := req.Method + " " + req.URL.Path
		status := "success"
		if err != nil || (resp != nil && resp.StatusCode >= 400) {
			status = "error"
			VMMMetrics.APIErrorsTotal.Add(req.Context(), 1,
				metric.WithAttributes(attribute.String("operation", operation)))
		}
		VMMMetrics.APIDuration.Record(req.Context(), time.Since(start).Seconds(),
			metric.WithAttributes(
				attribute.String("operation", operation),
				attribute.String("status", status),
			))
	}

	return resp, err
}

// NewVMM creates a Cloud Hypervisor client for an existing VMM socket
func NewVMM(socketPath string) (*VMM, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		// Disable keep-alives to prevent connection leaks.
		// Each NewVMM call creates a new transport, so connection pooling
		// just causes connections to accumulate until cloud-hypervisor
		// hits its connection limit.
		DisableKeepAlives: true,
	}

	httpClient := &http.Client{
		Transport: &metricsRoundTripper{base: transport, tracer: otel.Tracer("hypeman/vmm")},
		Timeout:   120 * time.Second,
	}

	client, err := NewClientWithResponses("http://localhost/api/v1",
		WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	return &VMM{
		ClientWithResponses: client,
		socketPath:          socketPath,
	}, nil
}

// StartProcess starts a Cloud Hypervisor VMM process with the given version
// It extracts the embedded binary if needed and starts the VMM as a daemon.
// Returns the process ID of the started Cloud Hypervisor process.
func StartProcess(ctx context.Context, p *paths.Paths, version CHVersion, socketPath string) (int, error) {
	return StartProcessWithArgs(ctx, p, version, socketPath, nil)
}

// StartProcessWithArgs starts a Cloud Hypervisor VMM process with additional command-line arguments.
// This is useful for testing or when you need to pass specific flags like verbosity.
func StartProcessWithArgs(ctx context.Context, p *paths.Paths, version CHVersion, socketPath string, extraArgs []string) (int, error) {
	// Get binary path (extracts if needed)
	binaryPath, err := GetBinaryPath(p, version)
	if err != nil {
		return 0, fmt.Errorf("get binary: %w", err)
	}

	// Check if socket is already in use
	if isSocketInUse(socketPath) {
		return 0, fmt.Errorf("socket already in use, VMM may be running at %s", socketPath)
	}

	// Remove stale socket if exists
	// Ignore error - if we can't remove it, CH will fail with clearer error
	os.Remove(socketPath)

	// Build command arguments
	args := []string{"--api-socket", socketPath}
	args = append(args, extraArgs...)

	// Redirect stdout/stderr to combined VMM log file (process won't block on I/O)
	instanceDir := filepath.Dir(socketPath)
	logsDir := filepath.Join(instanceDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return 0, fmt.Errorf("create logs directory: %w", err)
	}

	vmmLogFile, err := os.OpenFile(
		filepath.Join(logsDir, "vmm.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return 0, fmt.Errorf("create vmm log: %w", err)
	}
	// Note: This defer closes the parent's file descriptor after cmd.Start().
	// The child process receives a duplicated file descriptor during fork/exec,
	// so it can continue writing to the log file even after we close it here.
	defer vmmLogFile.Close()

	// Both stdout and stderr go to the same file
	var cmd *exec.Cmd
	for attempt := 1; attempt <= cloudHypervisorStartRetryAttempts; attempt++ {
		// Use Command (not CommandContext) so process survives parent context cancellation.
		cmd = exec.Command(binaryPath, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true, // Create new process group
		}
		cmd.Stdout = vmmLogFile
		cmd.Stderr = vmmLogFile

		err := cmd.Start()
		if err == nil {
			break
		}
		if !isTextFileBusyError(err) || attempt == cloudHypervisorStartRetryAttempts {
			return 0, fmt.Errorf("start cloud-hypervisor: %w", err)
		}
		time.Sleep(cloudHypervisorStartRetryDelay)
	}

	pid := cmd.Process.Pid

	// Wait for socket to be ready (use fresh context with timeout, not parent context).
	// CI can be heavily loaded; a larger budget avoids transient CH boot races.
	waitCtx, cancel := context.WithTimeout(context.Background(), cloudHypervisorSocketReadyTimeout)
	defer cancel()

	if err := waitForSocket(waitCtx, socketPath, cloudHypervisorSocketReadyTimeout); err != nil {
		// Read vmm.log to understand why socket wasn't created
		vmmLogPath := filepath.Join(logsDir, "vmm.log")
		if logData, readErr := os.ReadFile(vmmLogPath); readErr == nil && len(logData) > 0 {
			return 0, fmt.Errorf("%w; vmm.log: %s", err, string(logData))
		}
		return 0, err
	}

	return pid, nil
}

func isTextFileBusyError(err error) bool {
	return errors.Is(err, syscall.ETXTBSY) || strings.Contains(strings.ToLower(err.Error()), "text file busy")
}

// isSocketInUse checks if a Unix socket is actively being used
func isSocketInUse(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false // Socket doesn't exist or not listening
	}
	conn.Close()
	return true
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for socket")
		case <-ticker.C:
			if conn, err := net.Dial("unix", path); err == nil {
				conn.Close()
				return nil
			}
		}
	}
}
