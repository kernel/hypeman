// Package vmconfig defines the configuration schema passed from host to guest VM.
package vmconfig

// Config is the configuration passed to the guest init binary via config.json.
// This struct is serialized by the host (lib/instances/configdisk.go) and
// deserialized by the guest init binary (lib/system/init).
type Config struct {
	// Container execution parameters
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	Workdir    string   `json:"workdir"`

	// Environment variables
	Env map[string]string `json:"env"`

	// Network configuration
	NetworkEnabled bool   `json:"network_enabled"`
	GuestIP        string `json:"guest_ip,omitempty"`
	GuestCIDR      int    `json:"guest_cidr,omitempty"`
	GuestGW        string `json:"guest_gw,omitempty"`
	GuestDNS       string `json:"guest_dns,omitempty"`

	// GPU passthrough
	HasGPU bool `json:"has_gpu"`

	// Volume mounts
	VolumeMounts []VolumeMount `json:"volume_mounts,omitempty"`

	// Init mode: "exec" (default) or "systemd"
	InitMode string `json:"init_mode"`
}

// VolumeMount represents a volume mount configuration.
type VolumeMount struct {
	Device        string `json:"device"`
	Path          string `json:"path"`
	Mode          string `json:"mode"` // "ro", "rw", or "overlay"
	OverlayDevice string `json:"overlay_device,omitempty"`
}
