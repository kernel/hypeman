package snapshottransfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/nrednav/cuid2"
)

func TestUploadImportChunkOrderAndIdempotency(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1)

	chunk0 := []byte("aaaa")
	chunk1 := []byte("bbbb")
	session, err := mgr.CreateImportSession(context.Background(), CreateSessionRequest{
		Snapshot: SnapshotDescriptor{
			SourceSnapshotID:   "src-snap",
			SourceInstanceID:   "inst-1",
			SourceInstanceName: "inst-1",
			Kind:               snapshotstore.SnapshotKindStopped,
			SourceHypervisor:   hypervisor.TypeCloudHypervisor,
			CreatedAt:          time.Now(),
			SizeBytes:          int64(len(chunk0) + len(chunk1)),
		},
		Manifest: Manifest{
			Version:   1,
			ChunkSize: 4,
			DataSize:  int64(len(chunk0) + len(chunk1)),
			Chunks: []ChunkDescriptor{
				{Index: 0, Offset: 0, Size: int64(len(chunk0)), SHA256: sha256Hex(chunk0)},
				{Index: 1, Offset: int64(len(chunk0)), Size: int64(len(chunk1)), SHA256: sha256Hex(chunk1)},
			},
		},
		StoredMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateImportSession: %v", err)
	}

	err = mgr.UploadImportChunk(context.Background(), session.ID, 1, strings.NewReader(string(chunk1)))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected out-of-order upload to fail with conflict, got %v", err)
	}

	if err := mgr.UploadImportChunk(context.Background(), session.ID, 0, strings.NewReader(string(chunk0))); err != nil {
		t.Fatalf("upload chunk 0: %v", err)
	}
	if err := mgr.UploadImportChunk(context.Background(), session.ID, 0, strings.NewReader(string(chunk0))); err != nil {
		t.Fatalf("re-upload chunk 0 should be idempotent: %v", err)
	}
	if err := mgr.UploadImportChunk(context.Background(), session.ID, 1, strings.NewReader("zz")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected invalid request for wrong chunk size, got %v", err)
	}
	if err := mgr.UploadImportChunk(context.Background(), session.ID, 1, strings.NewReader(string(chunk1))); err != nil {
		t.Fatalf("upload chunk 1: %v", err)
	}

	got, err := mgr.GetImportSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetImportSession: %v", err)
	}
	if len(got.CommittedChunkIDs) != 2 || !containsChunk(got.CommittedChunkIDs, 0) || !containsChunk(got.CommittedChunkIDs, 1) {
		t.Fatalf("expected committed chunks [0,1], got %v", got.CommittedChunkIDs)
	}
}

func TestStartTransferPreflightFailureDoesNotCreateJob(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1).(*manager)

	const snapshotID = "snap-preflight"
	if err := seedSourceSnapshot(p, snapshotID, map[string][]byte{
		"file.txt": []byte("payload"),
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshot-import-sessions/preflight" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Error(w, `{"code":"conflict","message":"name exists"}`, http.StatusConflict)
	}))
	defer server.Close()

	_, err := mgr.StartTransfer(context.Background(), snapshotID, StartTransferRequest{
		DestinationURL: server.URL,
	}, "token")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected preflight conflict error, got %v", err)
	}

	jobs, err := mgr.ListTransfers(context.Background())
	if err != nil {
		t.Fatalf("ListTransfers: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected no transfer jobs after failed preflight, got %d", len(jobs))
	}
}

func TestPreflightImportStandbyCompatibilityMismatch(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1)

	sourceOS := "linux"
	if runtime.GOOS == "linux" {
		sourceOS = "darwin"
	}
	err := mgr.PreflightImport(context.Background(), PreflightRequest{
		Snapshot: SnapshotDescriptor{
			SourceSnapshotID:   "src-standby",
			SourceInstanceID:   "inst-1",
			SourceInstanceName: "inst-1",
			Kind:               snapshotstore.SnapshotKindStandby,
			SourceHypervisor:   hypervisor.TypeCloudHypervisor,
			CreatedAt:          time.Now(),
			Compat: SourceSnapshotCompat{
				Hypervisor:        hypervisor.TypeCloudHypervisor,
				PlatformOS:        sourceOS,
				PlatformArch:      runtime.GOARCH,
				KernelVersion:     "k1",
				HypervisorVersion: "h1",
			},
		},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected standby platform mismatch conflict, got %v", err)
	}
}

func TestPreflightImportStandbyCompatibilityPass(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1)

	hv := hypervisor.TypeCloudHypervisor
	if runtime.GOOS == "darwin" {
		hv = hypervisor.TypeVZ
	}
	kernelVersion := "test-kernel"
	kernelPath := p.SystemKernel(kernelVersion, normalizeKernelArch(runtime.GOARCH))
	if err := os.MkdirAll(filepath.Dir(kernelPath), 0o755); err != nil {
		t.Fatalf("mkdir kernel dir: %v", err)
	}
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel file: %v", err)
	}

	err := mgr.PreflightImport(context.Background(), PreflightRequest{
		Snapshot: SnapshotDescriptor{
			SourceSnapshotID:   "src-standby-pass",
			SourceInstanceID:   "inst-1",
			SourceInstanceName: "inst-1",
			Kind:               snapshotstore.SnapshotKindStandby,
			SourceHypervisor:   hv,
			CreatedAt:          time.Now(),
			Compat: SourceSnapshotCompat{
				Hypervisor:        hv,
				PlatformOS:        runtime.GOOS,
				PlatformArch:      runtime.GOARCH,
				KernelVersion:     kernelVersion,
				HypervisorVersion: "h1",
			},
		},
	})
	if err != nil {
		t.Fatalf("expected standby preflight to pass, got %v", err)
	}
}

func TestPreflightImportStandbyCompatibilityPassWithKernelArchAlias(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1)

	hv := hypervisor.TypeCloudHypervisor
	if runtime.GOOS == "darwin" {
		hv = hypervisor.TypeVZ
	}
	kernelVersion := "test-kernel-alias"
	kernelPath := p.SystemKernel(kernelVersion, normalizeKernelArch(runtime.GOARCH))
	if err := os.MkdirAll(filepath.Dir(kernelPath), 0o755); err != nil {
		t.Fatalf("mkdir kernel dir: %v", err)
	}
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel file: %v", err)
	}

	platformArch := "x86_64"
	if runtime.GOARCH == "arm64" {
		platformArch = "aarch64"
	}

	err := mgr.PreflightImport(context.Background(), PreflightRequest{
		Snapshot: SnapshotDescriptor{
			SourceSnapshotID:   "src-standby-pass-alias",
			SourceInstanceID:   "inst-1",
			SourceInstanceName: "inst-1",
			Kind:               snapshotstore.SnapshotKindStandby,
			SourceHypervisor:   hv,
			CreatedAt:          time.Now(),
			Compat: SourceSnapshotCompat{
				Hypervisor:        hv,
				PlatformOS:        runtime.GOOS,
				PlatformArch:      platformArch,
				KernelVersion:     kernelVersion,
				HypervisorVersion: "h1",
			},
		},
	})
	if err != nil {
		t.Fatalf("expected standby preflight with arch alias to pass, got %v", err)
	}
}

func TestCreateImportSessionRejectsUnsafeManifestPaths(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1)

	badPaths := []string{
		"",
		".",
		"/etc/passwd",
		"../escape",
		"safe/../../escape",
	}

	for i, badPath := range badPaths {
		_, err := mgr.CreateImportSession(context.Background(), CreateSessionRequest{
			Snapshot: SnapshotDescriptor{
				SourceSnapshotID:   fmt.Sprintf("src-bad-path-%d", i),
				SourceInstanceID:   "inst-1",
				SourceInstanceName: "inst-1",
				Kind:               snapshotstore.SnapshotKindStopped,
				SourceHypervisor:   hypervisor.TypeCloudHypervisor,
				CreatedAt:          time.Now(),
			},
			Manifest: Manifest{
				Version:   1,
				ChunkSize: 4096,
				DataSize:  0,
				Entries: []ManifestEntry{
					{Path: badPath, Type: EntryTypeFile, Mode: 0644, Size: 0},
				},
			},
			StoredMetadata: json.RawMessage(`{}`),
		})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("expected invalid request for path %q, got %v", badPath, err)
		}
	}
}

func TestCompleteImportSessionRejectsSymlinkTraversal(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1)

	session, err := mgr.CreateImportSession(context.Background(), CreateSessionRequest{
		Snapshot: SnapshotDescriptor{
			SourceSnapshotID:   "src-symlink-parent",
			SourceInstanceID:   "inst-1",
			SourceInstanceName: "inst-1",
			Name:               "symlink-parent",
			Kind:               snapshotstore.SnapshotKindStopped,
			SourceHypervisor:   hypervisor.TypeCloudHypervisor,
			CreatedAt:          time.Now(),
		},
		Manifest: Manifest{
			Version:   1,
			ChunkSize: 4096,
			DataSize:  0,
			Entries: []ManifestEntry{
				{Path: "safe", Type: EntryTypeDirectory, Mode: 0755},
				{Path: "safe/link", Type: EntryTypeSymlink, Mode: 0777, LinkTarget: "../../outside"},
				{Path: "safe/link/payload.bin", Type: EntryTypeFile, Mode: 0644, Size: 0},
			},
		},
		StoredMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateImportSession: %v", err)
	}

	_, err = mgr.CompleteImportSession(context.Background(), session.ID)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected invalid request for symlink parent traversal, got %v", err)
	}
}

func TestCompleteImportSessionRejectsFileTargetSymlink(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1)

	session, err := mgr.CreateImportSession(context.Background(), CreateSessionRequest{
		Snapshot: SnapshotDescriptor{
			SourceSnapshotID:   "src-symlink-target",
			SourceInstanceID:   "inst-1",
			SourceInstanceName: "inst-1",
			Name:               "symlink-target",
			Kind:               snapshotstore.SnapshotKindStopped,
			SourceHypervisor:   hypervisor.TypeCloudHypervisor,
			CreatedAt:          time.Now(),
		},
		Manifest: Manifest{
			Version:   1,
			ChunkSize: 4096,
			DataSize:  0,
			Entries: []ManifestEntry{
				{Path: "leaf", Type: EntryTypeSymlink, Mode: 0777, LinkTarget: "outside"},
				{Path: "leaf", Type: EntryTypeFile, Mode: 0644, Size: 0},
			},
		},
		StoredMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateImportSession: %v", err)
	}

	_, err = mgr.CompleteImportSession(context.Background(), session.ID)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected invalid request for symlink target write, got %v", err)
	}
}

func TestTransferEndToEndStoppedSnapshot(t *testing.T) {
	srcPaths := paths.New(t.TempDir())
	dstPaths := paths.New(t.TempDir())

	const sourceSnapshotID = "snap-e2e"
	if err := seedSourceSnapshot(srcPaths, sourceSnapshotID, map[string][]byte{
		"root.txt":          []byte("root-data"),
		"dir/nested.txt":    []byte("nested-data"),
		"dir/another.bin":   []byte("abcdefgh"),
		"empty/placeholder": {},
	}); err != nil {
		t.Fatalf("seed source snapshot: %v", err)
	}

	dstMgr := NewManager(dstPaths, 1)
	dstServer := newDestinationServer(t, dstMgr, "dst-token")
	defer dstServer.Close()

	srcMgr := NewManager(srcPaths, 1).(*manager)
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	if err := srcMgr.Start(workerCtx); err != nil {
		t.Fatalf("start source manager: %v", err)
	}

	job, err := srcMgr.StartTransfer(context.Background(), sourceSnapshotID, StartTransferRequest{
		DestinationURL: dstServer.URL,
	}, "dst-token")
	if err != nil {
		t.Fatalf("StartTransfer: %v", err)
	}

	final := waitForTransferTerminal(t, srcMgr, job.ID, 10*time.Second)
	if final.Status != StatusCompleted {
		t.Fatalf("expected completed transfer, got status=%s err=%v", final.Status, final.Error)
	}

	dstStore := snapshotstore.NewStore(dstPaths)
	records, err := dstStore.ListRecords()
	if err != nil {
		t.Fatalf("list destination snapshot records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one destination snapshot, got %d", len(records))
	}
	rec := records[0]
	if rec.Snapshot.Metadata[transferSourceSnapshotIDTag] != sourceSnapshotID {
		t.Fatalf("expected source snapshot tag=%s, got %q", sourceSnapshotID, rec.Snapshot.Metadata[transferSourceSnapshotIDTag])
	}

	got, err := os.ReadFile(filepath.Join(dstPaths.SnapshotGuestDir(rec.Snapshot.Id), "dir", "nested.txt"))
	if err != nil {
		t.Fatalf("read imported file: %v", err)
	}
	if string(got) != "nested-data" {
		t.Fatalf("imported payload mismatch: got=%q", string(got))
	}
}

func TestStartResumesInterruptedJobOnStartup(t *testing.T) {
	p := paths.New(t.TempDir())
	mgr := NewManager(p, 1).(*manager)

	rec := &TransferRecord{
		ID: cuid2.Generate(),
		Snapshot: SnapshotDescriptor{
			SourceSnapshotID:   "missing-snapshot",
			SourceInstanceID:   "inst-1",
			SourceInstanceName: "inst-1",
			Kind:               snapshotstore.SnapshotKindStopped,
			SourceHypervisor:   hypervisor.TypeCloudHypervisor,
			CreatedAt:          time.Now(),
		},
		DestinationURL:   "http://127.0.0.1:1",
		DestinationToken: "token",
		Status:           StatusRunning,
		CreatedAt:        time.Now().Add(-1 * time.Minute),
	}
	if err := mgr.store.SaveTransfer(rec); err != nil {
		t.Fatalf("save transfer record: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := mgr.store.LoadTransfer(rec.ID)
		if err != nil {
			t.Fatalf("load transfer: %v", err)
		}
		if cur.Status == StatusFailed {
			if cur.Error == "" {
				t.Fatalf("expected failure reason after resumed processing")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for resumed job to fail")
}

func waitForTransferTerminal(t *testing.T, mgr *manager, transferID string, timeout time.Duration) TransferJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := mgr.GetTransfer(context.Background(), transferID)
		if err != nil {
			t.Fatalf("GetTransfer: %v", err)
		}
		if isTransferTerminal(job.Status) {
			return *job
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for transfer %s terminal state", transferID)
	return TransferJob{}
}

func seedSourceSnapshot(p *paths.Paths, snapshotID string, files map[string][]byte) error {
	store := snapshotstore.NewStore(p)
	guestDir := p.SnapshotGuestDir(snapshotID)
	if err := os.MkdirAll(guestDir, 0o755); err != nil {
		return err
	}
	for rel, data := range files {
		full := filepath.Join(guestDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
	}
	size, err := snapshotstore.DirectoryFileSize(guestDir)
	if err != nil {
		return err
	}
	return store.SaveRecord(&snapshotstore.Record{
		Snapshot: snapshotstore.Snapshot{
			Id:               snapshotID,
			Name:             "transfer-test",
			Kind:             snapshotstore.SnapshotKindStopped,
			Metadata:         map[string]string{"test": "true"},
			SourceInstanceID: "inst-1",
			SourceName:       "inst-1",
			SourceHypervisor: hypervisor.TypeCloudHypervisor,
			CreatedAt:        time.Now(),
			SizeBytes:        size,
		},
		StoredMetadata: json.RawMessage(`{"KernelVersion":"k1","HypervisorVersion":"h1","HypervisorType":"cloud-hypervisor"}`),
	})
}

func newDestinationServer(t *testing.T, mgr Manager, expectedToken string) *httptest.Server {
	t.Helper()

	writeErr := func(w http.ResponseWriter, err error) {
		switch {
		case errors.Is(err, ErrInvalidRequest):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrSessionNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+expectedToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/snapshot-import-sessions/preflight":
			var req PreflightRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := mgr.PreflightImport(r.Context(), req); err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return

		case r.Method == http.MethodPost && r.URL.Path == "/snapshot-import-sessions":
			var req CreateSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			session, err := mgr.CreateImportSession(r.Context(), req)
			if err != nil {
				writeErr(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(session)
			return
		}

		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 || parts[0] != "snapshot-import-sessions" {
			http.NotFound(w, r)
			return
		}
		sessionID := parts[1]

		if len(parts) == 2 && r.Method == http.MethodGet {
			session, err := mgr.GetImportSession(r.Context(), sessionID)
			if err != nil {
				writeErr(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(session)
			return
		}

		if len(parts) == 3 && parts[2] == "complete" && r.Method == http.MethodPost {
			snap, err := mgr.CompleteImportSession(r.Context(), sessionID)
			if err != nil {
				writeErr(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(snap)
			return
		}

		if len(parts) == 3 && parts[2] == "cancel" && r.Method == http.MethodPost {
			if err := mgr.CancelImportSession(r.Context(), sessionID); err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if len(parts) == 4 && parts[2] == "chunks" && r.Method == http.MethodPut {
			idx, err := strconv.Atoi(parts[3])
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid chunk index: %s", parts[3]), http.StatusBadRequest)
				return
			}
			if err := mgr.UploadImportChunk(r.Context(), sessionID, idx, r.Body); err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		http.NotFound(w, r)
	}))
}

func containsChunk(ids []int, target int) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
