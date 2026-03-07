package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

type apiError struct {
	FaultMessage string `json:"fault_message"`
}

// Firecracker implements hypervisor.Hypervisor for the Firecracker VMM.
type Firecracker struct {
	socketPath string
	client     *http.Client
}

func New(socketPath string) (*Firecracker, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	return &Firecracker{
		socketPath: socketPath,
		client: &http.Client{
			Transport: transport,
			Timeout:   90 * time.Second,
		},
	}, nil
}

var _ hypervisor.Hypervisor = (*Firecracker)(nil)

func (f *Firecracker) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot:       true,
		SupportsHotplugMemory:  false,
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: false,
		SupportsDiskIOLimit:    true,
	}
}

func (f *Firecracker) DeleteVM(ctx context.Context) error {
	return f.postAction(ctx, "SendCtrlAltDel")
}

func (f *Firecracker) Shutdown(ctx context.Context) error {
	return hypervisor.ErrNotSupported
}

func (f *Firecracker) GetVMInfo(ctx context.Context) (*hypervisor.VMInfo, error) {
	body, err := f.do(ctx, http.MethodGet, "/", nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("get vm info: %w", err)
	}

	var info instanceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode vm info: %w", err)
	}

	state, err := mapVMState(info.State)
	if err != nil {
		return nil, err
	}

	return &hypervisor.VMInfo{State: state}, nil
}

func (f *Firecracker) Pause(ctx context.Context) error {
	_, err := f.do(ctx, http.MethodPatch, "/vm", vmState{State: "Paused"}, http.StatusNoContent)
	if err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	return nil
}

func (f *Firecracker) Resume(ctx context.Context) error {
	_, err := f.do(ctx, http.MethodPatch, "/vm", vmState{State: "Resumed"}, http.StatusNoContent)
	if err != nil {
		return fmt.Errorf("resume vm: %w", err)
	}
	return nil
}

func (f *Firecracker) Snapshot(ctx context.Context, destPath string) error {
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	params := toSnapshotCreateParams(destPath)
	if _, err := f.do(ctx, http.MethodPut, "/snapshot/create", params, http.StatusNoContent); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	return nil
}

func (f *Firecracker) ResizeMemory(ctx context.Context, bytes int64) error {
	return hypervisor.ErrNotSupported
}

func (f *Firecracker) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	return hypervisor.ErrNotSupported
}

func (f *Firecracker) configureForBoot(ctx context.Context, cfg hypervisor.VMConfig) error {
	if cfg.SerialLogPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.SerialLogPath), 0755); err != nil {
			return fmt.Errorf("create serial log directory: %w", err)
		}
		if _, err := f.do(ctx, http.MethodPut, "/serial", serialDevice{
			SerialOutPath: cfg.SerialLogPath,
		}, http.StatusNoContent); err != nil {
			// The /serial endpoint was added in Firecracker v1.14.0.
			// Keep this fallback for custom/older binaries that may not expose it.
			if !strings.Contains(err.Error(), "Invalid request method and/or path") {
				return fmt.Errorf("configure serial: %w", err)
			}
		}
	}

	if _, err := f.do(ctx, http.MethodPut, "/boot-source", toBootSource(cfg), http.StatusNoContent); err != nil {
		return fmt.Errorf("configure boot source: %w", err)
	}
	if _, err := f.do(ctx, http.MethodPut, "/machine-config", toMachineConfiguration(cfg), http.StatusNoContent); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}

	for _, driveCfg := range toDriveConfigs(cfg) {
		path := "/drives/" + url.PathEscape(driveCfg.DriveID)
		if _, err := f.do(ctx, http.MethodPut, path, driveCfg, http.StatusNoContent); err != nil {
			return fmt.Errorf("configure drive %s: %w", driveCfg.DriveID, err)
		}
	}

	for _, netCfg := range toNetworkInterfaces(cfg) {
		path := "/network-interfaces/" + url.PathEscape(netCfg.IfaceID)
		if _, err := f.do(ctx, http.MethodPut, path, netCfg, http.StatusNoContent); err != nil {
			return fmt.Errorf("configure network interface %s: %w", netCfg.IfaceID, err)
		}
	}

	vsockCfg := toVsockConfig(cfg)
	if vsockCfg != nil {
		if _, err := f.do(ctx, http.MethodPut, "/vsock", vsockCfg, http.StatusNoContent); err != nil {
			return fmt.Errorf("configure vsock: %w", err)
		}
	}

	return nil
}

func (f *Firecracker) instanceStart(ctx context.Context) error {
	return f.postAction(ctx, "InstanceStart")
}

func (f *Firecracker) loadSnapshot(ctx context.Context, snapshotDir string, networkOverrides []networkOverride) error {
	params := toSnapshotLoadParams(snapshotDir, networkOverrides)
	if _, err := f.do(ctx, http.MethodPut, "/snapshot/load", params, http.StatusNoContent); err != nil {
		return err
	}
	return nil
}

func (f *Firecracker) postAction(ctx context.Context, action string) error {
	_, err := f.do(ctx, http.MethodPut, "/actions", instanceActionInfo{ActionType: action}, http.StatusNoContent)
	if err != nil {
		return fmt.Errorf("firecracker action %s failed: %w", action, err)
	}
	return nil
}

func (f *Firecracker) do(ctx context.Context, method, path string, reqBody any, expectedStatus ...int) ([]byte, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	for _, status := range expectedStatus {
		if resp.StatusCode == status {
			return data, nil
		}
	}

	if len(data) > 0 {
		var apiErr apiError
		if err := json.Unmarshal(data, &apiErr); err == nil && apiErr.FaultMessage != "" {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, apiErr.FaultMessage)
		}
	}
	return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(data))
}

const (
	// State strings returned by Firecracker GET "/".
	// Source of truth:
	// - src/vmm/src/vmm_config/instance_info.rs (Display impl for VmState)
	// - src/firecracker/swagger/firecracker.yaml (InstanceInfo.state enum)
	firecrackerStateNotStarted = "Not started"
	firecrackerStateRunning    = "Running"
	firecrackerStatePaused     = "Paused"
)

func mapVMState(state string) (hypervisor.VMState, error) {
	switch state {
	case firecrackerStateNotStarted:
		return hypervisor.StateCreated, nil
	case firecrackerStateRunning:
		return hypervisor.StateRunning, nil
	case firecrackerStatePaused:
		return hypervisor.StatePaused, nil
	default:
		return "", fmt.Errorf("unknown firecracker state: %q", state)
	}
}
