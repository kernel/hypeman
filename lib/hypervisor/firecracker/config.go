package firecracker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman/lib/hypervisor"
)

const (
	snapshotStateFile   = "state"
	snapshotMemoryFile  = "memory"
	restoreMetadataFile = "firecracker-config.json"
)

type bootSource struct {
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
	KernelImagePath string `json:"kernel_image_path"`
}

type machineConfiguration struct {
	MemSizeMib      int64 `json:"mem_size_mib"`
	TrackDirtyPages bool  `json:"track_dirty_pages,omitempty"`
	VcpuCount       int   `json:"vcpu_count"`
}

type drive struct {
	DriveID      string       `json:"drive_id"`
	IsRootDevice bool         `json:"is_root_device"`
	IsReadOnly   bool         `json:"is_read_only,omitempty"`
	PathOnHost   string       `json:"path_on_host,omitempty"`
	RateLimiter  *rateLimiter `json:"rate_limiter,omitempty"`
}

type networkInterface struct {
	GuestMAC      string       `json:"guest_mac,omitempty"`
	HostDevName   string       `json:"host_dev_name"`
	IfaceID       string       `json:"iface_id"`
	RxRateLimiter *rateLimiter `json:"rx_rate_limiter,omitempty"`
	TxRateLimiter *rateLimiter `json:"tx_rate_limiter,omitempty"`
}

type vsock struct {
	GuestCID int64  `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type serialDevice struct {
	SerialOutPath string `json:"serial_out_path"`
}

type instanceActionInfo struct {
	ActionType string `json:"action_type"`
}

type vmState struct {
	State string `json:"state"`
}

type snapshotCreateParams struct {
	MemFilePath  string `json:"mem_file_path"`
	SnapshotPath string `json:"snapshot_path"`
	SnapshotType string `json:"snapshot_type,omitempty"`
}

type snapshotLoadParams struct {
	MemFilePath         string            `json:"mem_file_path,omitempty"`
	SnapshotPath        string            `json:"snapshot_path"`
	EnableDiffSnapshots bool              `json:"enable_diff_snapshots,omitempty"`
	ResumeVM            bool              `json:"resume_vm,omitempty"`
	NetworkOverrides    []networkOverride `json:"network_overrides,omitempty"`
}

type networkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

type rateLimiter struct {
	Bandwidth *tokenBucket `json:"bandwidth,omitempty"`
}

type tokenBucket struct {
	OneTimeBurst *int64 `json:"one_time_burst,omitempty"`
	RefillTime   int64  `json:"refill_time"`
	Size         int64  `json:"size"`
}

type instanceInfo struct {
	State string `json:"state"`
}

type restoreMetadata struct {
	NetworkOverrides      []networkOverride `json:"network_overrides,omitempty"`
	SnapshotSourceDataDir string            `json:"snapshot_source_data_dir,omitempty"`
}

func toBootSource(cfg hypervisor.VMConfig) bootSource {
	return bootSource{
		BootArgs:        cfg.KernelArgs,
		InitrdPath:      cfg.InitrdPath,
		KernelImagePath: cfg.KernelPath,
	}
}

func toMachineConfiguration(cfg hypervisor.VMConfig) machineConfiguration {
	vcpus := cfg.VCPUs
	if vcpus <= 0 {
		vcpus = 1
	}
	return machineConfiguration{
		MemSizeMib:      bytesToMiB(cfg.MemoryBytes),
		TrackDirtyPages: true,
		VcpuCount:       vcpus,
	}
}

func toDriveConfigs(cfg hypervisor.VMConfig) []drive {
	out := make([]drive, 0, len(cfg.Disks))
	for i, d := range cfg.Disks {
		id := fmt.Sprintf("disk%d", i)
		if i == 0 {
			id = "rootfs"
		}
		out = append(out, drive{
			DriveID:      id,
			IsRootDevice: i == 0,
			IsReadOnly:   d.Readonly,
			PathOnHost:   d.Path,
			RateLimiter:  toRateLimiter(d.IOBps, d.IOBurstBps),
		})
	}
	return out
}

func toNetworkInterfaces(cfg hypervisor.VMConfig) []networkInterface {
	out := make([]networkInterface, 0, len(cfg.Networks))
	for i, n := range cfg.Networks {
		out = append(out, networkInterface{
			GuestMAC:      n.MAC,
			HostDevName:   n.TAPDevice,
			IfaceID:       fmt.Sprintf("eth%d", i),
			RxRateLimiter: toRateLimiter(n.DownloadBps, n.DownloadBps),
			TxRateLimiter: toRateLimiter(n.UploadBps, n.UploadBps),
		})
	}
	return out
}

func toVsockConfig(cfg hypervisor.VMConfig) *vsock {
	if cfg.VsockCID <= 0 || cfg.VsockSocket == "" {
		return nil
	}
	return &vsock{
		GuestCID: cfg.VsockCID,
		UDSPath:  cfg.VsockSocket,
	}
}

func toRateLimiter(limit int64, burst int64) *rateLimiter {
	if limit <= 0 {
		return nil
	}

	var oneTimeBurst *int64
	if burst > limit {
		extra := burst - limit
		oneTimeBurst = &extra
	}

	return &rateLimiter{
		Bandwidth: &tokenBucket{
			OneTimeBurst: oneTimeBurst,
			RefillTime:   1000,
			Size:         limit,
		},
	}
}

func toSnapshotCreateParams(snapshotDir string) snapshotCreateParams {
	return snapshotCreateParams{
		MemFilePath:  snapshotMemoryPath(snapshotDir),
		SnapshotPath: snapshotStatePath(snapshotDir),
		SnapshotType: "Full",
	}
}

func toSnapshotLoadParams(snapshotDir string, networkOverrides []networkOverride) snapshotLoadParams {
	return snapshotLoadParams{
		MemFilePath:         snapshotMemoryPath(snapshotDir),
		SnapshotPath:        snapshotStatePath(snapshotDir),
		EnableDiffSnapshots: true,
		ResumeVM:            false,
		NetworkOverrides:    networkOverrides,
	}
}

func snapshotStatePath(snapshotDir string) string {
	return filepath.Join(snapshotDir, snapshotStateFile)
}

func snapshotMemoryPath(snapshotDir string) string {
	return filepath.Join(snapshotDir, snapshotMemoryFile)
}

func saveRestoreMetadata(instanceDir string, networkConfigs []networkInterface) error {
	meta := restoreMetadata{
		NetworkOverrides: make([]networkOverride, 0, len(networkConfigs)),
	}
	for _, netCfg := range networkConfigs {
		meta.NetworkOverrides = append(meta.NetworkOverrides, networkOverride{
			IfaceID:     netCfg.IfaceID,
			HostDevName: netCfg.HostDevName,
		})
	}

	return saveRestoreMetadataState(instanceDir, &meta)
}

func saveRestoreMetadataState(instanceDir string, meta *restoreMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal firecracker restore metadata: %w", err)
	}
	path := filepath.Join(instanceDir, restoreMetadataFile)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write firecracker restore metadata: %w", err)
	}
	return nil
}

func loadRestoreMetadata(instanceDir string) (*restoreMetadata, error) {
	path := filepath.Join(instanceDir, restoreMetadataFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &restoreMetadata{}, nil
		}
		return nil, fmt.Errorf("read firecracker restore metadata: %w", err)
	}

	var meta restoreMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal firecracker restore metadata: %w", err)
	}
	return &meta, nil
}

func bytesToMiB(bytes int64) int64 {
	if bytes <= 0 {
		return 128
	}
	const mib = 1024 * 1024
	out := bytes / mib
	if bytes%mib != 0 {
		out++
	}
	if out < 1 {
		out = 1
	}
	return out
}
