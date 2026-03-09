package snapshottransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/nrednav/cuid2"
)

const (
	transferSourceSnapshotIDTag = "hypeman.transfer_source_snapshot_id"
)

type Manager interface {
	Start(ctx context.Context) error
	StartTransfer(ctx context.Context, snapshotID string, req StartTransferRequest, destinationToken string) (*TransferJob, error)
	ListTransfers(ctx context.Context) ([]TransferJob, error)
	GetTransfer(ctx context.Context, transferID string) (*TransferJob, error)
	CancelTransfer(ctx context.Context, transferID string) error

	PreflightImport(ctx context.Context, req PreflightRequest) error
	CreateImportSession(ctx context.Context, req CreateSessionRequest) (*ImportSession, error)
	GetImportSession(ctx context.Context, sessionID string) (*ImportSession, error)
	UploadImportChunk(ctx context.Context, sessionID string, chunkIndex int, body io.Reader) error
	CompleteImportSession(ctx context.Context, sessionID string) (*snapshotstore.Snapshot, error)
	CancelImportSession(ctx context.Context, sessionID string) error
}

type manager struct {
	paths         *paths.Paths
	store         *Store
	httpClient    *http.Client
	maxConcurrent int
	chunkSize     int64

	mu          sync.Mutex
	started     bool
	queue       chan string
	cancelFuncs map[string]context.CancelFunc
	workerCtx   context.Context
	workerStop  context.CancelFunc
}

func NewManager(p *paths.Paths, maxConcurrent int) Manager {
	if maxConcurrent < 1 {
		maxConcurrent = 2
	}
	return &manager{
		paths:         p,
		store:         NewStore(p),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		maxConcurrent: maxConcurrent,
		chunkSize:     DefaultChunkSize,
		cancelFuncs:   make(map[string]context.CancelFunc),
	}
}

func (m *manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m.workerCtx = workerCtx
	m.workerStop = cancel
	m.queue = make(chan string, 128)
	m.started = true
	for i := 0; i < m.maxConcurrent; i++ {
		go m.worker(workerCtx)
	}
	m.mu.Unlock()

	records, err := m.store.ListTransfers()
	if err != nil {
		return err
	}
	for i := range records {
		rec := records[i]
		if isTransferTerminal(rec.Status) {
			continue
		}
		rec.Status = StatusQueued
		rec.Error = ""
		_ = m.store.SaveTransfer(&rec)
		m.enqueue(rec.ID)
	}
	return nil
}

func (m *manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.queue:
			if err := m.processTransfer(ctx, id); err != nil {
				rec, loadErr := m.store.LoadTransfer(id)
				if loadErr != nil {
					continue
				}
				if rec.Status == StatusCancelled {
					continue
				}
				now := time.Now()
				rec.Status = StatusFailed
				rec.Error = err.Error()
				rec.CompletedAt = &now
				_ = m.store.SaveTransfer(rec)
				logger.FromContext(ctx).ErrorContext(ctx, "snapshot transfer failed", "transfer_id", id, "error", err)
			}
		}
	}
}

func (m *manager) enqueue(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return
	}
	select {
	case m.queue <- id:
	default:
		go func() { m.queue <- id }()
	}
}

func (m *manager) processTransfer(ctx context.Context, transferID string) error {
	rec, err := m.store.LoadTransfer(transferID)
	if err != nil {
		return err
	}
	if rec.Cancelled || rec.Status == StatusCancelled {
		return nil
	}

	now := time.Now()
	rec.Status = StatusRunning
	if rec.StartedAt == nil {
		rec.StartedAt = &now
	}
	rec.Error = ""
	if err := m.store.SaveTransfer(rec); err != nil {
		return err
	}

	tctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancelFuncs[transferID] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancelFuncs, transferID)
		m.mu.Unlock()
		cancel()
	}()

	snapshotRec, guestDir, err := m.loadSnapshotRecord(rec.Snapshot.SourceSnapshotID)
	if err != nil {
		return err
	}
	manifest, extentRefs, err := m.ensureTransferManifest(rec, guestDir)
	if err != nil {
		return err
	}

	session, err := m.ensureDestinationSession(tctx, rec, snapshotRec.StoredMetadata, manifest)
	if err != nil {
		return err
	}
	rec.DestinationSessionID = session.ID
	rec.DataSize = manifest.DataSize
	rec.ChunksTotal = len(manifest.Chunks)

	committed := make(map[int]bool, len(session.CommittedChunkIDs))
	for _, idx := range session.CommittedChunkIDs {
		committed[idx] = true
	}
	rec.ChunksTransferred, rec.BytesTransferred = chunkProgressFromCommitted(manifest, committed)
	if err := m.store.SaveTransfer(rec); err != nil {
		return err
	}

	for _, chunk := range manifest.Chunks {
		if committed[chunk.Index] {
			continue
		}
		select {
		case <-tctx.Done():
			return tctx.Err()
		default:
		}

		buf := bytes.NewBuffer(make([]byte, 0, chunk.Size))
		if err := readDataRange(guestDir, extentRefs, chunk.Offset, chunk.Size, buf); err != nil {
			return err
		}
		if err := m.uploadDestinationChunk(tctx, rec, chunk, buf.Bytes()); err != nil {
			return err
		}

		committed[chunk.Index] = true
		rec.ChunksTransferred++
		rec.BytesTransferred += chunk.Size
		if err := m.store.SaveTransfer(rec); err != nil {
			return err
		}
	}

	if err := m.completeDestinationSession(tctx, rec); err != nil {
		return err
	}

	now = time.Now()
	rec.Status = StatusCompleted
	rec.CompletedAt = &now
	rec.Error = ""
	return m.store.SaveTransfer(rec)
}

func (m *manager) ensureTransferManifest(rec *TransferRecord, guestDir string) (Manifest, []extentRef, error) {
	manifest, err := m.store.LoadTransferManifest(rec.ID)
	if err == nil {
		extents := manifestExtents(*manifest)
		return *manifest, extents, nil
	}
	if !errors.Is(err, ErrTransferNotFound) {
		return Manifest{}, nil, err
	}

	built, extents, err := BuildManifest(guestDir, m.chunkSize)
	if err != nil {
		return Manifest{}, nil, err
	}
	if err := m.store.SaveTransferManifest(rec.ID, built); err != nil {
		return Manifest{}, nil, err
	}
	return built, extents, nil
}

func (m *manager) ensureDestinationSession(ctx context.Context, rec *TransferRecord, storedMetadata json.RawMessage, manifest Manifest) (*ImportSession, error) {
	if rec.DestinationSessionID != "" {
		session, err := m.getRemoteSession(ctx, rec, rec.DestinationSessionID)
		if err == nil {
			return session, nil
		}
	}

	payload := CreateSessionRequest{Snapshot: rec.Snapshot, Manifest: manifest, StoredMetadata: storedMetadata}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(rec.DestinationURL, "/") + "/snapshot-import-sessions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rec.DestinationToken)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("create destination session failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var session ImportSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("decode destination session: %w", err)
	}
	return &session, nil
}

func (m *manager) uploadDestinationChunk(ctx context.Context, rec *TransferRecord, chunk ChunkDescriptor, data []byte) error {
	url := strings.TrimRight(rec.DestinationURL, "/") + "/snapshot-import-sessions/" + rec.DestinationSessionID + "/chunks/" + fmt.Sprintf("%d", chunk.Index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+rec.DestinationToken)
	req.Header.Set("X-Hypeman-Chunk-Sha256", chunk.SHA256)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload chunk %d failed: status=%d body=%s", chunk.Index, resp.StatusCode, string(body))
	}
	return nil
}

func (m *manager) completeDestinationSession(ctx context.Context, rec *TransferRecord) error {
	url := strings.TrimRight(rec.DestinationURL, "/") + "/snapshot-import-sessions/" + rec.DestinationSessionID + "/complete"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rec.DestinationToken)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("complete destination session failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func (m *manager) getRemoteSession(ctx context.Context, rec *TransferRecord, sessionID string) (*ImportSession, error) {
	url := strings.TrimRight(rec.DestinationURL, "/") + "/snapshot-import-sessions/" + sessionID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rec.DestinationToken)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get destination session failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var out ImportSession
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (m *manager) StartTransfer(ctx context.Context, snapshotID string, req StartTransferRequest, destinationToken string) (*TransferJob, error) {
	if strings.TrimSpace(req.DestinationURL) == "" {
		return nil, fmt.Errorf("%w: destination_url is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(destinationToken) == "" {
		return nil, fmt.Errorf("%w: destination token is required", ErrInvalidRequest)
	}

	record, _, err := m.loadSnapshotRecord(snapshotID)
	if err != nil {
		return nil, err
	}
	desc := snapshotDescriptorFromRecord(record)
	if err := m.remotePreflight(ctx, req.DestinationURL, destinationToken, desc); err != nil {
		return nil, err
	}

	now := time.Now()
	rec := &TransferRecord{
		ID:               cuid2.Generate(),
		Snapshot:         desc,
		DestinationURL:   strings.TrimRight(req.DestinationURL, "/"),
		DestinationToken: destinationToken,
		Status:           StatusQueued,
		CreatedAt:        now,
	}
	if err := m.store.SaveTransfer(rec); err != nil {
		return nil, err
	}
	m.enqueue(rec.ID)

	job := toTransferJob(rec)
	return &job, nil
}

func (m *manager) remotePreflight(ctx context.Context, destinationURL, destinationToken string, desc SnapshotDescriptor) error {
	payload := PreflightRequest{Snapshot: desc}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := strings.TrimRight(destinationURL, "/") + "/snapshot-import-sessions/preflight"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+destinationToken)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusBadRequest {
			return fmt.Errorf("%w: destination preflight failed: %s", ErrConflict, strings.TrimSpace(string(body)))
		}
		return fmt.Errorf("destination preflight failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func (m *manager) ListTransfers(ctx context.Context) ([]TransferJob, error) {
	_ = ctx
	recs, err := m.store.ListTransfers()
	if err != nil {
		return nil, err
	}
	out := make([]TransferJob, 0, len(recs))
	for i := range recs {
		rec := recs[i]
		out = append(out, toTransferJob(&rec))
	}
	return out, nil
}

func (m *manager) GetTransfer(ctx context.Context, transferID string) (*TransferJob, error) {
	_ = ctx
	rec, err := m.store.LoadTransfer(transferID)
	if err != nil {
		return nil, err
	}
	job := toTransferJob(rec)
	return &job, nil
}

func (m *manager) CancelTransfer(ctx context.Context, transferID string) error {
	rec, err := m.store.LoadTransfer(transferID)
	if err != nil {
		return err
	}
	if isTransferTerminal(rec.Status) {
		return nil
	}
	now := time.Now()
	rec.Cancelled = true
	rec.Status = StatusCancelled
	rec.CompletedAt = &now
	rec.Error = ""
	if err := m.store.SaveTransfer(rec); err != nil {
		return err
	}

	m.mu.Lock()
	cancel := m.cancelFuncs[transferID]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if rec.DestinationSessionID != "" {
		_ = m.cancelRemoteSession(ctx, rec)
	}
	return nil
}

func (m *manager) cancelRemoteSession(ctx context.Context, rec *TransferRecord) error {
	url := strings.TrimRight(rec.DestinationURL, "/") + "/snapshot-import-sessions/" + rec.DestinationSessionID + "/cancel"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rec.DestinationToken)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("cancel destination session failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func isTransferTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

func chunkProgressFromCommitted(manifest Manifest, committed map[int]bool) (int, int64) {
	var chunks int
	var bytes int64
	for _, ch := range manifest.Chunks {
		if committed[ch.Index] {
			chunks++
			bytes += ch.Size
		}
	}
	return chunks, bytes
}

func manifestExtents(manifest Manifest) []extentRef {
	out := make([]extentRef, 0, 128)
	for _, entry := range manifest.Entries {
		if entry.Type != EntryTypeFile {
			continue
		}
		for _, ex := range entry.Extents {
			out = append(out, extentRef{path: entry.Path, fileOffset: ex.FileOffset, length: ex.Length, dataOffset: ex.DataOffset})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dataOffset == out[j].dataOffset {
			return out[i].path < out[j].path
		}
		return out[i].dataOffset < out[j].dataOffset
	})
	return out
}

func (m *manager) loadSnapshotRecord(snapshotID string) (*snapshotstore.Record, string, error) {
	store := snapshotstore.NewStore(m.paths)
	rec, err := store.LoadRecord(snapshotID)
	if err != nil {
		if errors.Is(err, snapshotstore.ErrNotFound) {
			return nil, "", snapshotstore.ErrNotFound
		}
		return nil, "", err
	}
	guestDir := m.paths.SnapshotGuestDir(snapshotID)
	if stat, err := os.Stat(guestDir); err != nil || !stat.IsDir() {
		if err == nil {
			err = fmt.Errorf("snapshot guest dir is not a directory")
		}
		return nil, "", fmt.Errorf("snapshot guest payload missing: %w", err)
	}
	return rec, guestDir, nil
}

func snapshotDescriptorFromRecord(rec *snapshotstore.Record) SnapshotDescriptor {
	compat := SourceSnapshotCompat{
		Hypervisor:   rec.Snapshot.SourceHypervisor,
		PlatformOS:   runtime.GOOS,
		PlatformArch: runtime.GOARCH,
	}
	var meta struct {
		KernelVersion     string          `json:"KernelVersion"`
		HypervisorVersion string          `json:"HypervisorVersion"`
		HypervisorType    hypervisor.Type `json:"HypervisorType"`
	}
	if len(rec.StoredMetadata) > 0 {
		_ = json.Unmarshal(rec.StoredMetadata, &meta)
		if meta.KernelVersion != "" {
			compat.KernelVersion = meta.KernelVersion
		}
		if meta.HypervisorVersion != "" {
			compat.HypervisorVersion = meta.HypervisorVersion
		}
		if meta.HypervisorType != "" {
			compat.Hypervisor = meta.HypervisorType
		}
	}
	return SnapshotDescriptor{
		SourceSnapshotID:   rec.Snapshot.Id,
		SourceInstanceID:   rec.Snapshot.SourceInstanceID,
		SourceInstanceName: rec.Snapshot.SourceName,
		Name:               rec.Snapshot.Name,
		Kind:               rec.Snapshot.Kind,
		SourceHypervisor:   rec.Snapshot.SourceHypervisor,
		Metadata:           tags.Clone(rec.Snapshot.Metadata),
		CreatedAt:          rec.Snapshot.CreatedAt,
		SizeBytes:          rec.Snapshot.SizeBytes,
		Compat:             compat,
	}
}

func (m *manager) PreflightImport(ctx context.Context, req PreflightRequest) error {
	_ = ctx
	desc := req.Snapshot
	if desc.SourceSnapshotID == "" {
		return fmt.Errorf("%w: source_snapshot_id is required", ErrInvalidRequest)
	}
	if desc.Kind != snapshotstore.SnapshotKindStandby && desc.Kind != snapshotstore.SnapshotKindStopped {
		return fmt.Errorf("%w: unsupported snapshot kind %q", ErrInvalidRequest, desc.Kind)
	}

	if err := m.checkConflicts(desc); err != nil {
		return err
	}
	if desc.Kind == snapshotstore.SnapshotKindStandby {
		if err := m.checkStandbyCompatibility(desc); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) checkConflicts(desc SnapshotDescriptor) error {
	store := snapshotstore.NewStore(m.paths)
	recs, err := store.ListRecords()
	if err != nil {
		return err
	}
	for i := range recs {
		r := recs[i]
		if r.Snapshot.Metadata != nil && r.Snapshot.Metadata[transferSourceSnapshotIDTag] == desc.SourceSnapshotID {
			return fmt.Errorf("%w: snapshot already imported from source snapshot %s", ErrConflict, desc.SourceSnapshotID)
		}
	}
	if err := store.EnsureNameAvailable(desc.SourceInstanceID, desc.Name); err != nil {
		if errors.Is(err, snapshotstore.ErrNameExists) {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return err
	}
	sessions, err := m.store.ListSessions()
	if err != nil {
		return err
	}
	for i := range sessions {
		s := sessions[i]
		if s.SourceSnapshotID == desc.SourceSnapshotID && s.Status != SessionStatusCancelled && s.Status != SessionStatusCompleted {
			return fmt.Errorf("%w: active import session exists for source snapshot %s", ErrConflict, desc.SourceSnapshotID)
		}
	}
	return nil
}

func (m *manager) checkStandbyCompatibility(desc SnapshotDescriptor) error {
	c := desc.Compat
	if c.PlatformOS == "" || c.PlatformArch == "" {
		return fmt.Errorf("%w: standby transfer requires platform metadata", ErrInvalidRequest)
	}
	if c.PlatformOS != runtime.GOOS || !isSamePlatformArch(c.PlatformArch, runtime.GOARCH) {
		return fmt.Errorf("%w: standby snapshot platform mismatch source=%s/%s destination=%s/%s", ErrConflict, c.PlatformOS, c.PlatformArch, runtime.GOOS, runtime.GOARCH)
	}
	if !isHypervisorSupportedOnPlatform(c.Hypervisor, runtime.GOOS) {
		return fmt.Errorf("%w: standby snapshot hypervisor %s unsupported on destination platform", ErrConflict, c.Hypervisor)
	}
	if c.KernelVersion == "" || c.HypervisorVersion == "" {
		return fmt.Errorf("%w: standby transfer requires kernel and hypervisor version metadata", ErrInvalidRequest)
	}
	kernelPath := m.paths.SystemKernel(c.KernelVersion, normalizeKernelArch(c.PlatformArch))
	if _, err := os.Stat(kernelPath); err != nil {
		return fmt.Errorf("%w: required kernel version %s not available on destination", ErrConflict, c.KernelVersion)
	}

	if localVersion, ok := m.localHypervisorVersion(c.Hypervisor); ok && localVersion != c.HypervisorVersion {
		return fmt.Errorf("%w: standby hypervisor version mismatch source=%s destination=%s", ErrConflict, c.HypervisorVersion, localVersion)
	}
	return nil
}

func isSamePlatformArch(a, b string) bool {
	return normalizePlatformArch(a) == normalizePlatformArch(b)
}

func normalizePlatformArch(arch string) string {
	switch arch {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return arch
	}
}

func normalizeKernelArch(platformArch string) string {
	switch normalizePlatformArch(platformArch) {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return platformArch
	}
}

func isHypervisorSupportedOnPlatform(hv hypervisor.Type, goos string) bool {
	switch goos {
	case "darwin":
		return hv == hypervisor.TypeVZ
	case "linux":
		return hv == hypervisor.TypeCloudHypervisor || hv == hypervisor.TypeQEMU || hv == hypervisor.TypeFirecracker
	default:
		return false
	}
}

func (m *manager) localHypervisorVersion(hv hypervisor.Type) (string, bool) {
	entries, err := os.ReadDir(m.paths.GuestsDir())
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := m.paths.InstanceMetadata(e.Name())
		b, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta struct {
			HypervisorType    hypervisor.Type `json:"HypervisorType"`
			HypervisorVersion string          `json:"HypervisorVersion"`
		}
		if err := json.Unmarshal(b, &meta); err != nil {
			continue
		}
		if meta.HypervisorType == hv && meta.HypervisorVersion != "" {
			return meta.HypervisorVersion, true
		}
	}
	return "", false
}

func (m *manager) CreateImportSession(ctx context.Context, req CreateSessionRequest) (*ImportSession, error) {
	if err := m.PreflightImport(ctx, PreflightRequest{Snapshot: req.Snapshot}); err != nil {
		return nil, err
	}
	if req.Manifest.Version != 1 {
		return nil, fmt.Errorf("%w: unsupported manifest version", ErrInvalidRequest)
	}
	if len(req.Manifest.Chunks) == 0 && req.Manifest.DataSize > 0 {
		return nil, fmt.Errorf("%w: manifest chunk list is empty", ErrInvalidRequest)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	sessions, err := m.store.ListSessions()
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		s := sessions[i]
		if s.SourceSnapshotID != req.Snapshot.SourceSnapshotID {
			continue
		}
		if s.Status == SessionStatusCreated || s.Status == SessionStatusReceiving {
			sess := toImportSession(&s)
			return &sess, nil
		}
		return nil, fmt.Errorf("%w: previous session for source snapshot is terminal", ErrConflict)
	}

	now := time.Now()
	rec := &ImportSessionRecord{
		ID:               cuid2.Generate(),
		SourceSnapshotID: req.Snapshot.SourceSnapshotID,
		Snapshot:         req.Snapshot,
		Manifest:         req.Manifest,
		StoredMetadata:   req.StoredMetadata,
		CommittedChunks:  map[int]bool{},
		Status:           SessionStatusCreated,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := m.store.SaveSession(rec); err != nil {
		return nil, err
	}
	sess := toImportSession(rec)
	return &sess, nil
}

func (m *manager) GetImportSession(ctx context.Context, sessionID string) (*ImportSession, error) {
	_ = ctx
	rec, err := m.store.LoadSession(sessionID)
	if err != nil {
		return nil, err
	}
	sess := toImportSession(rec)
	return &sess, nil
}

func (m *manager) UploadImportChunk(ctx context.Context, sessionID string, chunkIndex int, body io.Reader) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.store.LoadSession(sessionID)
	if err != nil {
		return err
	}
	if rec.Status == SessionStatusCancelled || rec.Status == SessionStatusCompleted {
		return fmt.Errorf("%w: session is in terminal state", ErrConflict)
	}
	if chunkIndex < 0 || chunkIndex >= len(rec.Manifest.Chunks) {
		return fmt.Errorf("%w: chunk index out of range", ErrInvalidRequest)
	}
	if rec.CommittedChunks[chunkIndex] {
		return nil
	}
	if chunkIndex > 0 && !rec.CommittedChunks[chunkIndex-1] {
		return fmt.Errorf("%w: out-of-order chunk commit rejected for index %d", ErrConflict, chunkIndex)
	}

	chunk := rec.Manifest.Chunks[chunkIndex]
	data, err := io.ReadAll(io.LimitReader(body, chunk.Size+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != chunk.Size {
		return fmt.Errorf("%w: chunk %d size mismatch expected=%d got=%d", ErrInvalidRequest, chunkIndex, chunk.Size, len(data))
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != chunk.SHA256 {
		return fmt.Errorf("%w: chunk %d checksum mismatch", ErrInvalidRequest, chunkIndex)
	}
	chunkPath := m.paths.SnapshotImportSessionChunk(sessionID, chunkIndex)
	if err := os.MkdirAll(filepath.Dir(chunkPath), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(chunkPath, data, 0600); err != nil {
		return err
	}
	rec.CommittedChunks[chunkIndex] = true
	rec.Status = SessionStatusReceiving
	rec.UpdatedAt = time.Now()
	return m.store.SaveSession(rec)
}

func (m *manager) CompleteImportSession(ctx context.Context, sessionID string) (*snapshotstore.Snapshot, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.store.LoadSession(sessionID)
	if err != nil {
		return nil, err
	}
	if rec.Status == SessionStatusCancelled {
		return nil, fmt.Errorf("%w: session cancelled", ErrConflict)
	}
	if rec.Status == SessionStatusCompleted && rec.ImportedSnapshotID != "" {
		store := snapshotstore.NewStore(m.paths)
		snap, err := store.Get(rec.ImportedSnapshotID)
		if err == nil {
			return snap, nil
		}
	}
	for _, ch := range rec.Manifest.Chunks {
		if !rec.CommittedChunks[ch.Index] {
			return nil, fmt.Errorf("%w: missing chunk %d", ErrConflict, ch.Index)
		}
	}

	snap, err := m.materializeSnapshot(rec)
	if err != nil {
		return nil, err
	}
	rec.Status = SessionStatusCompleted
	rec.ImportedSnapshotID = snap.Id
	rec.UpdatedAt = time.Now()
	if err := m.store.SaveSession(rec); err != nil {
		return nil, err
	}
	return snap, nil
}

func (m *manager) CancelImportSession(ctx context.Context, sessionID string) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.store.LoadSession(sessionID)
	if err != nil {
		return err
	}
	if rec.Status == SessionStatusCompleted {
		return fmt.Errorf("%w: cannot cancel completed session", ErrConflict)
	}
	rec.Status = SessionStatusCancelled
	rec.UpdatedAt = time.Now()
	if err := os.RemoveAll(m.paths.SnapshotImportSessionChunksDir(sessionID)); err != nil {
		return err
	}
	return m.store.SaveSession(rec)
}

func (m *manager) materializeSnapshot(session *ImportSessionRecord) (*snapshotstore.Snapshot, error) {
	store := snapshotstore.NewStore(m.paths)

	if err := store.EnsureNameAvailable(session.Snapshot.SourceInstanceID, session.Snapshot.Name); err != nil {
		if errors.Is(err, snapshotstore.ErrNameExists) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, err
	}

	snapshotID := cuid2.Generate()
	guestDir := m.paths.SnapshotGuestDir(snapshotID)
	if err := os.MkdirAll(guestDir, 0755); err != nil {
		return nil, fmt.Errorf("create imported snapshot guest dir: %w", err)
	}

	for _, entry := range session.Manifest.Entries {
		target := filepath.Join(guestDir, entry.Path)
		switch entry.Type {
		case EntryTypeDirectory:
			if err := os.MkdirAll(target, os.FileMode(entry.Mode)); err != nil {
				return nil, fmt.Errorf("create directory %s: %w", entry.Path, err)
			}
		case EntryTypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			if err := os.Symlink(entry.LinkTarget, target); err != nil {
				return nil, fmt.Errorf("create symlink %s: %w", entry.Path, err)
			}
		case EntryTypeFile:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(entry.Mode))
			if err != nil {
				return nil, fmt.Errorf("create file %s: %w", entry.Path, err)
			}
			if err := f.Truncate(entry.Size); err != nil {
				f.Close()
				return nil, err
			}
			f.Close()
		default:
			return nil, fmt.Errorf("%w: unknown entry type %s", ErrInvalidRequest, entry.Type)
		}
	}

	for _, entry := range session.Manifest.Entries {
		if entry.Type != EntryTypeFile || len(entry.Extents) == 0 {
			continue
		}
		target := filepath.Join(guestDir, entry.Path)
		f, err := os.OpenFile(target, os.O_WRONLY, os.FileMode(entry.Mode))
		if err != nil {
			return nil, err
		}
		for _, ex := range entry.Extents {
			if _, err := f.Seek(ex.FileOffset, io.SeekStart); err != nil {
				f.Close()
				return nil, err
			}
			if err := m.copySessionDataToWriter(session, ex.DataOffset, ex.Length, f); err != nil {
				f.Close()
				return nil, err
			}
		}
		f.Close()
	}

	metadata := tags.Clone(session.Snapshot.Metadata)
	if metadata == nil {
		metadata = tags.Metadata{}
	}
	metadata[transferSourceSnapshotIDTag] = session.Snapshot.SourceSnapshotID

	record := &snapshotstore.Record{
		Snapshot: snapshotstore.Snapshot{
			Id:               snapshotID,
			Name:             session.Snapshot.Name,
			Kind:             session.Snapshot.Kind,
			Metadata:         metadata,
			SourceInstanceID: session.Snapshot.SourceInstanceID,
			SourceName:       session.Snapshot.SourceInstanceName,
			SourceHypervisor: session.Snapshot.SourceHypervisor,
			CreatedAt:        session.Snapshot.CreatedAt,
			SizeBytes:        session.Snapshot.SizeBytes,
		},
		StoredMetadata: session.StoredMetadata,
	}
	if err := store.SaveRecord(record); err != nil {
		return nil, err
	}
	return &record.Snapshot, nil
}

func (m *manager) copySessionDataToWriter(session *ImportSessionRecord, dataOffset, length int64, w io.Writer) error {
	if length == 0 {
		return nil
	}
	end := dataOffset + length
	for _, ch := range session.Manifest.Chunks {
		chunkStart := ch.Offset
		chunkEnd := ch.Offset + ch.Size
		if chunkEnd <= dataOffset || chunkStart >= end {
			continue
		}
		segStart := max64(dataOffset, chunkStart)
		segEnd := min64(end, chunkEnd)
		segLen := segEnd - segStart
		if segLen <= 0 {
			continue
		}
		chunkPath := m.paths.SnapshotImportSessionChunk(session.ID, ch.Index)
		f, err := os.Open(chunkPath)
		if err != nil {
			return err
		}
		if _, err := f.Seek(segStart-chunkStart, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		if _, err := io.CopyN(w, f, segLen); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return nil
}
