package uffdpager

import "errors"

const (
	BackendFile = "file"
	BackendUFFD = "uffd"

	defaultCacheMaxBytes = int64(4 << 30)
)

// ErrSessionNotFound reports that a pager no longer has the requested session,
// so a completion request had nothing to act on.
var ErrSessionNotFound = errors.New("uffd pager session not found")

// CreateSessionRequest describes one Firecracker UFFD restore session.
type CreateSessionRequest struct {
	SessionID         string `json:"session_id,omitempty"`
	InstanceID        string `json:"instance_id"`
	BackingMemoryPath string `json:"backing_memory_path"`
	CacheKey          string `json:"cache_key"`
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

	CacheBytes          int64 `json:"cache_bytes"`
	CacheMax            int64 `json:"cache_max"`
	CacheItems          int   `json:"cache_items"`
	CacheHits           int64 `json:"cache_hits"`
	CacheMisses         int64 `json:"cache_misses"`
	CacheShards         int   `json:"cache_shards"`
	CacheLookupNanos    int64 `json:"cache_lookup_nanos"`
	CacheLookupMaxNanos int64 `json:"cache_lookup_max_nanos"`
	CacheAddNanos       int64 `json:"cache_add_nanos"`
	CacheAddMaxNanos    int64 `json:"cache_add_max_nanos"`

	Faults           int64 `json:"faults"`
	BackingBytesRead int64 `json:"backing_bytes_read"`
	Copies           int64 `json:"copies"`
	CopyErrors       int64 `json:"copy_errors"`

	ActiveFaults        int64 `json:"active_faults"`
	MaxConcurrentFaults int64 `json:"max_concurrent_faults"`
	FaultNanos          int64 `json:"fault_nanos"`
	FaultMaxNanos       int64 `json:"fault_max_nanos"`
	ReadPageNanos       int64 `json:"read_page_nanos"`
	ReadPageMaxNanos    int64 `json:"read_page_max_nanos"`
	BackingReadNanos    int64 `json:"backing_read_nanos"`
	BackingReadMaxNanos int64 `json:"backing_read_max_nanos"`
	CopyNanos           int64 `json:"copy_nanos"`
	CopyMaxNanos        int64 `json:"copy_max_nanos"`
}

func normalizeCacheMaxBytes(v int64) int64 {
	if v <= 0 {
		return defaultCacheMaxBytes
	}
	return v
}
