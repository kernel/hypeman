package snapshot

import (
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/tags"
)

// SnapshotKind determines how snapshot data is captured and restored.
type SnapshotKind string

const (
	// SnapshotKindStandby captures snapshot-based standby state (memory/device/disk).
	SnapshotKindStandby SnapshotKind = "Standby"
	// SnapshotKindStopped captures stopped-state disk+metadata only.
	SnapshotKindStopped SnapshotKind = "Stopped"
)

// Snapshot is a centrally stored immutable snapshot resource.
type Snapshot struct {
	Id                    string        `json:"id"`
	Name                  string        `json:"name"`
	Kind                  SnapshotKind  `json:"kind"`
	Metadata              tags.Metadata `json:"metadata,omitempty"`
	SourceInstanceID      string        `json:"source_instance_id"`
	SourceName            string        `json:"source_instance_name"`
	SourceHypervisor      hypervisor.Type
	CreatedAt             time.Time                  `json:"created_at"`
	SizeBytes             int64                      `json:"size_bytes"`
	CompressionState      string                     `json:"compression_state,omitempty"`
	CompressionError      string                     `json:"compression_error,omitempty"`
	Compression           *SnapshotCompressionConfig `json:"compression,omitempty"`
	CompressedSizeBytes   *int64                     `json:"compressed_size_bytes,omitempty"`
	UncompressedSizeBytes *int64                     `json:"uncompressed_size_bytes,omitempty"`
}

// SnapshotCompressionAlgorithm defines supported compression algorithms.
type SnapshotCompressionAlgorithm string

const (
	SnapshotCompressionAlgorithmZstd SnapshotCompressionAlgorithm = "zstd"
	SnapshotCompressionAlgorithmLz4  SnapshotCompressionAlgorithm = "lz4"
)

const (
	SnapshotCompressionStateNone        = "none"
	SnapshotCompressionStateCompressing = "compressing"
	SnapshotCompressionStateCompressed  = "compressed"
	SnapshotCompressionStateError       = "error"
)

// SnapshotCompressionConfig defines requested or effective compression config.
type SnapshotCompressionConfig struct {
	Enabled   bool                         `json:"enabled"`
	Algorithm SnapshotCompressionAlgorithm `json:"algorithm,omitempty"`
	Level     *int                         `json:"level,omitempty"`
}

// ListSnapshotsFilter contains optional filters for listing snapshots.
type ListSnapshotsFilter struct {
	SourceInstanceID *string
	Kind             *SnapshotKind
	Name             *string
	Metadata         tags.Metadata
}

// Matches returns true if the given snapshot satisfies all filter criteria.
func (f *ListSnapshotsFilter) Matches(snapshot *Snapshot) bool {
	if f == nil {
		return true
	}
	if f.SourceInstanceID != nil && snapshot.SourceInstanceID != *f.SourceInstanceID {
		return false
	}
	if f.Kind != nil && snapshot.Kind != *f.Kind {
		return false
	}
	if f.Name != nil && snapshot.Name != *f.Name {
		return false
	}
	if !tags.Matches(snapshot.Metadata, f.Metadata) {
		return false
	}
	return true
}
