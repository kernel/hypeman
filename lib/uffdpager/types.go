package uffdpager

const (
	BackendFile = "file"
	BackendUFFD = "uffd"

	defaultCacheMaxBytes = int64(4 << 30)
)

// OverlayPage replaces a single snapshot memory page for one restore session.
type OverlayPage struct {
	GuestMemoryOffset int64  `json:"guest_memory_offset"`
	Path              string `json:"path"`
}

// CreateSessionRequest describes one Firecracker UFFD restore session.
type CreateSessionRequest struct {
	SessionID         string        `json:"session_id,omitempty"`
	InstanceID        string        `json:"instance_id"`
	BackingMemoryPath string        `json:"backing_memory_path"`
	CacheKey          string        `json:"cache_key"`
	Overlays          []OverlayPage `json:"overlays,omitempty"`
}

// CreateSessionResponse returns the per-session socket Firecracker should use
// as mem_backend.backend_path.
type CreateSessionResponse struct {
	SessionID      string `json:"session_id"`
	UFFDSocketPath string `json:"uffd_socket_path"`
	PagerVersion   string `json:"pager_version"`
}

type HealthResponse struct {
	Version        string `json:"version"`
	Draining       bool   `json:"draining"`
	ActiveSessions int    `json:"active_sessions"`
}

type Stats struct {
	Version        string `json:"version"`
	Draining       bool   `json:"draining"`
	ActiveSessions int    `json:"active_sessions"`

	CacheBytes  int64 `json:"cache_bytes"`
	CacheMax    int64 `json:"cache_max"`
	CacheItems  int   `json:"cache_items"`
	CacheHits   int64 `json:"cache_hits"`
	CacheMisses int64 `json:"cache_misses"`

	Faults           int64 `json:"faults"`
	OverlayFaults    int64 `json:"overlay_faults"`
	BackingBytesRead int64 `json:"backing_bytes_read"`
	Copies           int64 `json:"copies"`
	CopyErrors       int64 `json:"copy_errors"`
}

func normalizeCacheMaxBytes(v int64) int64 {
	if v <= 0 {
		return defaultCacheMaxBytes
	}
	return v
}
