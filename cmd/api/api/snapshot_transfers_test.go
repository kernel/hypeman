package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
)

func TestStartSnapshotTransferBodyRequired(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.StartSnapshotTransfer(context.Background(), oapi.StartSnapshotTransferRequestObject{
		SnapshotId: "snap-1",
		Params: oapi.StartSnapshotTransferParams{
			XHypemanDestinationToken: "token",
		},
	})
	if err != nil {
		t.Fatalf("StartSnapshotTransfer returned error: %v", err)
	}
	if _, ok := resp.(oapi.StartSnapshotTransfer400JSONResponse); !ok {
		t.Fatalf("expected 400 response, got %T", resp)
	}
}

func TestStartSnapshotTransferPreflightConflict(t *testing.T) {
	svc := newTestService(t)
	const snapshotID = "snap-api-preflight"
	if err := seedSnapshotForAPI(paths.New(svc.Config.DataDir), snapshotID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/snapshot-import-sessions/preflight" {
			http.Error(w, `{"code":"conflict","message":"already exists"}`, http.StatusConflict)
			return
		}
		http.NotFound(w, r)
	}))
	defer dest.Close()

	resp, err := svc.StartSnapshotTransfer(context.Background(), oapi.StartSnapshotTransferRequestObject{
		SnapshotId: snapshotID,
		Params: oapi.StartSnapshotTransferParams{
			XHypemanDestinationToken: "token",
		},
		Body: &oapi.StartSnapshotTransferRequest{
			DestinationUrl: dest.URL,
		},
	})
	if err != nil {
		t.Fatalf("StartSnapshotTransfer returned error: %v", err)
	}
	if _, ok := resp.(oapi.StartSnapshotTransfer409JSONResponse); !ok {
		t.Fatalf("expected 409 response, got %T", resp)
	}
}

func TestSnapshotImportSessionEndpoints(t *testing.T) {
	svc := newTestService(t)

	chunk0 := []byte("aaaa")
	chunk1 := []byte("bbbb")
	createReq := oapi.CreateSnapshotImportSessionRequestObject{
		Body: &oapi.SnapshotImportSessionCreateRequest{
			Snapshot: oapi.SnapshotDescriptor{
				SourceSnapshotId:   "src-api",
				SourceInstanceId:   "inst-api",
				SourceInstanceName: "inst-api",
				Kind:               oapi.SnapshotKindStopped,
				SourceHypervisor:   oapi.SnapshotDescriptorSourceHypervisorCloudHypervisor,
				CreatedAt:          time.Now(),
				SizeBytes:          int64(len(chunk0) + len(chunk1)),
				Compat: oapi.SourceSnapshotCompat{
					Hypervisor:   oapi.CloudHypervisor,
					PlatformOs:   "linux",
					PlatformArch: "amd64",
				},
			},
			Manifest: oapi.SnapshotImportManifest{
				Version:   1,
				ChunkSize: 4,
				DataSize:  int64(len(chunk0) + len(chunk1)),
				Chunks: []oapi.SnapshotChunkDescriptor{
					{Index: 0, Offset: 0, Size: int64(len(chunk0)), Sha256: sha256Hex(chunk0)},
					{Index: 1, Offset: int64(len(chunk0)), Size: int64(len(chunk1)), Sha256: sha256Hex(chunk1)},
				},
				Entries: []oapi.SnapshotImportManifestEntry{},
			},
			StoredMetadata: map[string]interface{}{
				"KernelVersion": "k1",
			},
		},
	}

	createResp, err := svc.CreateSnapshotImportSession(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateSnapshotImportSession returned error: %v", err)
	}
	created, ok := createResp.(oapi.CreateSnapshotImportSession201JSONResponse)
	if !ok {
		t.Fatalf("expected 201 create response, got %T", createResp)
	}

	uploadResp, err := svc.UploadSnapshotImportChunk(context.Background(), oapi.UploadSnapshotImportChunkRequestObject{
		SessionId:  created.Id,
		ChunkIndex: 1,
		Body:       strings.NewReader(string(chunk1)),
	})
	if err != nil {
		t.Fatalf("UploadSnapshotImportChunk returned error: %v", err)
	}
	if _, ok := uploadResp.(oapi.UploadSnapshotImportChunk409JSONResponse); !ok {
		t.Fatalf("expected 409 for out-of-order chunk upload, got %T", uploadResp)
	}

	for i, chunk := range [][]byte{chunk0, chunk1} {
		uploadResp, err := svc.UploadSnapshotImportChunk(context.Background(), oapi.UploadSnapshotImportChunkRequestObject{
			SessionId:  created.Id,
			ChunkIndex: i,
			Body:       strings.NewReader(string(chunk)),
		})
		if err != nil {
			t.Fatalf("UploadSnapshotImportChunk(%d) returned error: %v", i, err)
		}
		if _, ok := uploadResp.(oapi.UploadSnapshotImportChunk204Response); !ok {
			t.Fatalf("expected 204 for chunk %d, got %T", i, uploadResp)
		}
	}

	completeResp, err := svc.CompleteSnapshotImportSession(context.Background(), oapi.CompleteSnapshotImportSessionRequestObject{
		SessionId: created.Id,
	})
	if err != nil {
		t.Fatalf("CompleteSnapshotImportSession returned error: %v", err)
	}
	if _, ok := completeResp.(oapi.CompleteSnapshotImportSession201JSONResponse); !ok {
		t.Fatalf("expected 201 complete response, got %T", completeResp)
	}
}

func seedSnapshotForAPI(p *paths.Paths, snapshotID string) error {
	store := snapshotstore.NewStore(p)
	guestDir := p.SnapshotGuestDir(snapshotID)
	if err := os.MkdirAll(guestDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(guestDir, "payload.txt"), []byte("payload"), 0o644); err != nil {
		return err
	}
	size, err := snapshotstore.DirectoryFileSize(guestDir)
	if err != nil {
		return err
	}
	return store.SaveRecord(&snapshotstore.Record{
		Snapshot: snapshotstore.Snapshot{
			Id:               snapshotID,
			Name:             "api-seed",
			Kind:             snapshotstore.SnapshotKindStopped,
			Metadata:         map[string]string{"seed": "true"},
			SourceInstanceID: "inst-api",
			SourceName:       "inst-api",
			SourceHypervisor: hypervisor.TypeCloudHypervisor,
			CreatedAt:        time.Now(),
			SizeBytes:        size,
		},
		StoredMetadata: json.RawMessage(`{"KernelVersion":"k1","HypervisorVersion":"h1","HypervisorType":"cloud-hypervisor"}`),
	})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
