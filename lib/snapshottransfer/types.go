package snapshottransfer

import (
	"encoding/json"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/kernel/hypeman/lib/tags"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

const (
	SessionStatusCreated   = "created"
	SessionStatusReceiving = "receiving"
	SessionStatusCompleted = "completed"
	SessionStatusCancelled = "cancelled"
)

const (
	EntryTypeDirectory = "directory"
	EntryTypeFile      = "file"
	EntryTypeSymlink   = "symlink"
)

const (
	DefaultChunkSize int64 = 4 * 1024 * 1024
)

// SourceSnapshotCompat contains compatibility-critical fields derived from source stored metadata.
type SourceSnapshotCompat struct {
	KernelVersion     string          `json:"kernel_version"`
	HypervisorVersion string          `json:"hypervisor_version"`
	Hypervisor        hypervisor.Type `json:"hypervisor"`
	PlatformOS        string          `json:"platform_os"`
	PlatformArch      string          `json:"platform_arch"`
}

// SnapshotDescriptor is the immutable snapshot identity and metadata transferred between servers.
type SnapshotDescriptor struct {
	SourceSnapshotID   string                     `json:"source_snapshot_id"`
	SourceInstanceID   string                     `json:"source_instance_id"`
	SourceInstanceName string                     `json:"source_instance_name"`
	Name               string                     `json:"name"`
	Kind               snapshotstore.SnapshotKind `json:"kind"`
	SourceHypervisor   hypervisor.Type            `json:"source_hypervisor"`
	Tags               tags.Tags                  `json:"tags,omitempty"`
	CreatedAt          time.Time                  `json:"created_at"`
	SizeBytes          int64                      `json:"size_bytes"`
	Compat             SourceSnapshotCompat       `json:"compat"`
}

// DataExtent represents a sparse data extent of a regular file.
type DataExtent struct {
	FileOffset int64 `json:"file_offset"`
	Length     int64 `json:"length"`
	DataOffset int64 `json:"data_offset"`
}

// ManifestEntry describes one filesystem entry inside the snapshot guest payload.
type ManifestEntry struct {
	Path       string       `json:"path"`
	Type       string       `json:"type"`
	Mode       uint32       `json:"mode"`
	Size       int64        `json:"size,omitempty"`
	LinkTarget string       `json:"link_target,omitempty"`
	Extents    []DataExtent `json:"extents,omitempty"`
}

// ChunkDescriptor describes one chunk in the data stream over all extents.
type ChunkDescriptor struct {
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is deterministic and stable for a snapshot payload.
type Manifest struct {
	Version   int               `json:"version"`
	ChunkSize int64             `json:"chunk_size"`
	DataSize  int64             `json:"data_size"`
	Entries   []ManifestEntry   `json:"entries"`
	Chunks    []ChunkDescriptor `json:"chunks"`
}

// TransferRecord is source-side persisted state for async transfer.
type TransferRecord struct {
	ID                   string             `json:"id"`
	Snapshot             SnapshotDescriptor `json:"snapshot"`
	DestinationURL       string             `json:"destination_url"`
	DestinationToken     string             `json:"destination_token"`
	DestinationSessionID string             `json:"destination_session_id,omitempty"`
	Status               string             `json:"status"`
	Error                string             `json:"error,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	StartedAt            *time.Time         `json:"started_at,omitempty"`
	CompletedAt          *time.Time         `json:"completed_at,omitempty"`
	DataSize             int64              `json:"data_size"`
	ChunksTotal          int                `json:"chunks_total"`
	ChunksTransferred    int                `json:"chunks_transferred"`
	BytesTransferred     int64              `json:"bytes_transferred"`
	Cancelled            bool               `json:"cancelled"`
}

// TransferJob is API representation of source transfer.
type TransferJob struct {
	ID                   string     `json:"id"`
	SnapshotID           string     `json:"snapshot_id"`
	DestinationURL       string     `json:"destination_url"`
	DestinationSessionID *string    `json:"destination_session_id,omitempty"`
	Status               string     `json:"status"`
	Error                *string    `json:"error,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
	ChunksTotal          int        `json:"chunks_total"`
	ChunksTransferred    int        `json:"chunks_transferred"`
	BytesTransferred     int64      `json:"bytes_transferred"`
	DataSize             int64      `json:"data_size"`
}

// StartTransferRequest starts source-side transfer orchestration.
type StartTransferRequest struct {
	DestinationURL string `json:"destination_url"`
}

// PreflightRequest validates destination-side acceptance before source enqueues async job.
type PreflightRequest struct {
	Snapshot SnapshotDescriptor `json:"snapshot"`
}

// CreateSessionRequest creates or resumes a destination import session.
type CreateSessionRequest struct {
	Snapshot       SnapshotDescriptor `json:"snapshot"`
	Manifest       Manifest           `json:"manifest"`
	StoredMetadata json.RawMessage    `json:"stored_metadata"`
}

// ImportSessionRecord is destination-side persisted import session state.
type ImportSessionRecord struct {
	ID                 string             `json:"id"`
	SourceSnapshotID   string             `json:"source_snapshot_id"`
	Snapshot           SnapshotDescriptor `json:"snapshot"`
	Manifest           Manifest           `json:"manifest"`
	StoredMetadata     json.RawMessage    `json:"stored_metadata"`
	CommittedChunks    map[int]bool       `json:"committed_chunks"`
	Status             string             `json:"status"`
	ImportedSnapshotID string             `json:"imported_snapshot_id,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

// ImportSession is API representation of destination session.
type ImportSession struct {
	ID                 string    `json:"id"`
	SourceSnapshotID   string    `json:"source_snapshot_id"`
	Status             string    `json:"status"`
	CommittedChunkIDs  []int     `json:"committed_chunk_ids"`
	ChunksTotal        int       `json:"chunks_total"`
	DataSize           int64     `json:"data_size"`
	ImportedSnapshotID *string   `json:"imported_snapshot_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func toTransferJob(rec *TransferRecord) TransferJob {
	var errPtr *string
	if rec.Error != "" {
		err := rec.Error
		errPtr = &err
	}
	var sess *string
	if rec.DestinationSessionID != "" {
		s := rec.DestinationSessionID
		sess = &s
	}
	return TransferJob{
		ID:                   rec.ID,
		SnapshotID:           rec.Snapshot.SourceSnapshotID,
		DestinationURL:       rec.DestinationURL,
		DestinationSessionID: sess,
		Status:               rec.Status,
		Error:                errPtr,
		CreatedAt:            rec.CreatedAt,
		StartedAt:            rec.StartedAt,
		CompletedAt:          rec.CompletedAt,
		ChunksTotal:          rec.ChunksTotal,
		ChunksTransferred:    rec.ChunksTransferred,
		BytesTransferred:     rec.BytesTransferred,
		DataSize:             rec.DataSize,
	}
}

func toImportSession(rec *ImportSessionRecord) ImportSession {
	ids := make([]int, 0, len(rec.CommittedChunks))
	for i := range rec.CommittedChunks {
		ids = append(ids, i)
	}
	var imported *string
	if rec.ImportedSnapshotID != "" {
		s := rec.ImportedSnapshotID
		imported = &s
	}
	return ImportSession{
		ID:                 rec.ID,
		SourceSnapshotID:   rec.SourceSnapshotID,
		Status:             rec.Status,
		CommittedChunkIDs:  ids,
		ChunksTotal:        len(rec.Manifest.Chunks),
		DataSize:           rec.Manifest.DataSize,
		ImportedSnapshotID: imported,
		CreatedAt:          rec.CreatedAt,
		UpdatedAt:          rec.UpdatedAt,
	}
}
