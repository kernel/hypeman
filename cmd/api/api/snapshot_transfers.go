package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/kernel/hypeman/lib/snapshottransfer"
)

func (s *ApiService) StartSnapshotTransfer(ctx context.Context, request oapi.StartSnapshotTransferRequestObject) (oapi.StartSnapshotTransferResponseObject, error) {
	if request.Body == nil {
		return oapi.StartSnapshotTransfer400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}

	job, err := s.SnapshotTransfer.StartTransfer(ctx, request.SnapshotId, snapshottransfer.StartTransferRequest{
		DestinationURL: request.Body.DestinationUrl,
	}, request.Params.XHypemanDestinationToken)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrInvalidRequest):
			return oapi.StartSnapshotTransfer400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		case errors.Is(err, snapshotstore.ErrNotFound):
			return oapi.StartSnapshotTransfer404JSONResponse{Code: "not_found", Message: "snapshot not found"}, nil
		case errors.Is(err, snapshottransfer.ErrConflict):
			return oapi.StartSnapshotTransfer409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to start snapshot transfer", "error", err, "snapshot_id", request.SnapshotId)
			return oapi.StartSnapshotTransfer500JSONResponse{Code: "internal_error", Message: "failed to start snapshot transfer"}, nil
		}
	}

	return oapi.StartSnapshotTransfer202JSONResponse(toOAPISnapshotTransfer(*job)), nil
}

func (s *ApiService) ListSnapshotTransfers(ctx context.Context, request oapi.ListSnapshotTransfersRequestObject) (oapi.ListSnapshotTransfersResponseObject, error) {
	_ = request
	jobs, err := s.SnapshotTransfer.ListTransfers(ctx)
	if err != nil {
		log := logger.FromContext(ctx)
		log.ErrorContext(ctx, "failed to list snapshot transfers", "error", err)
		return oapi.ListSnapshotTransfers500JSONResponse{Code: "internal_error", Message: "failed to list snapshot transfers"}, nil
	}
	out := make([]oapi.SnapshotTransfer, 0, len(jobs))
	for i := range jobs {
		out = append(out, toOAPISnapshotTransfer(jobs[i]))
	}
	return oapi.ListSnapshotTransfers200JSONResponse(out), nil
}

func (s *ApiService) GetSnapshotTransfer(ctx context.Context, request oapi.GetSnapshotTransferRequestObject) (oapi.GetSnapshotTransferResponseObject, error) {
	job, err := s.SnapshotTransfer.GetTransfer(ctx, request.TransferId)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrTransferNotFound):
			return oapi.GetSnapshotTransfer404JSONResponse{Code: "not_found", Message: "snapshot transfer not found"}, nil
		default:
			log.ErrorContext(ctx, "failed to get snapshot transfer", "error", err, "transfer_id", request.TransferId)
			return oapi.GetSnapshotTransfer500JSONResponse{Code: "internal_error", Message: "failed to get snapshot transfer"}, nil
		}
	}
	return oapi.GetSnapshotTransfer200JSONResponse(toOAPISnapshotTransfer(*job)), nil
}

func (s *ApiService) CancelSnapshotTransfer(ctx context.Context, request oapi.CancelSnapshotTransferRequestObject) (oapi.CancelSnapshotTransferResponseObject, error) {
	err := s.SnapshotTransfer.CancelTransfer(ctx, request.TransferId)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrTransferNotFound):
			return oapi.CancelSnapshotTransfer404JSONResponse{Code: "not_found", Message: "snapshot transfer not found"}, nil
		default:
			log.ErrorContext(ctx, "failed to cancel snapshot transfer", "error", err, "transfer_id", request.TransferId)
			return oapi.CancelSnapshotTransfer500JSONResponse{Code: "internal_error", Message: "failed to cancel snapshot transfer"}, nil
		}
	}
	return oapi.CancelSnapshotTransfer204Response{}, nil
}

func (s *ApiService) PreflightSnapshotImportSession(ctx context.Context, request oapi.PreflightSnapshotImportSessionRequestObject) (oapi.PreflightSnapshotImportSessionResponseObject, error) {
	if request.Body == nil {
		return oapi.PreflightSnapshotImportSession400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}
	err := s.SnapshotTransfer.PreflightImport(ctx, snapshottransfer.PreflightRequest{
		Snapshot: fromOAPISnapshotDescriptor(request.Body.Snapshot),
	})
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrInvalidRequest):
			return oapi.PreflightSnapshotImportSession400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		case errors.Is(err, snapshottransfer.ErrConflict):
			return oapi.PreflightSnapshotImportSession409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "snapshot import preflight failed", "error", err)
			return oapi.PreflightSnapshotImportSession500JSONResponse{Code: "internal_error", Message: "snapshot import preflight failed"}, nil
		}
	}
	return oapi.PreflightSnapshotImportSession204Response{}, nil
}

func (s *ApiService) CreateSnapshotImportSession(ctx context.Context, request oapi.CreateSnapshotImportSessionRequestObject) (oapi.CreateSnapshotImportSessionResponseObject, error) {
	if request.Body == nil {
		return oapi.CreateSnapshotImportSession400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}

	storedMetadata, err := json.Marshal(request.Body.StoredMetadata)
	if err != nil {
		return oapi.CreateSnapshotImportSession400JSONResponse{Code: "invalid_request", Message: "stored_metadata must be valid JSON object"}, nil
	}

	session, err := s.SnapshotTransfer.CreateImportSession(ctx, snapshottransfer.CreateSessionRequest{
		Snapshot:       fromOAPISnapshotDescriptor(request.Body.Snapshot),
		Manifest:       fromOAPIManifest(request.Body.Manifest),
		StoredMetadata: storedMetadata,
	})
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrInvalidRequest):
			return oapi.CreateSnapshotImportSession400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		case errors.Is(err, snapshottransfer.ErrConflict):
			return oapi.CreateSnapshotImportSession409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to create snapshot import session", "error", err)
			return oapi.CreateSnapshotImportSession500JSONResponse{Code: "internal_error", Message: "failed to create snapshot import session"}, nil
		}
	}
	return oapi.CreateSnapshotImportSession201JSONResponse(toOAPISnapshotImportSession(*session)), nil
}

func (s *ApiService) GetSnapshotImportSession(ctx context.Context, request oapi.GetSnapshotImportSessionRequestObject) (oapi.GetSnapshotImportSessionResponseObject, error) {
	session, err := s.SnapshotTransfer.GetImportSession(ctx, request.SessionId)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrSessionNotFound):
			return oapi.GetSnapshotImportSession404JSONResponse{Code: "not_found", Message: "snapshot import session not found"}, nil
		default:
			log.ErrorContext(ctx, "failed to get snapshot import session", "error", err, "session_id", request.SessionId)
			return oapi.GetSnapshotImportSession500JSONResponse{Code: "internal_error", Message: "failed to get snapshot import session"}, nil
		}
	}
	return oapi.GetSnapshotImportSession200JSONResponse(toOAPISnapshotImportSession(*session)), nil
}

func (s *ApiService) UploadSnapshotImportChunk(ctx context.Context, request oapi.UploadSnapshotImportChunkRequestObject) (oapi.UploadSnapshotImportChunkResponseObject, error) {
	if request.Body == nil {
		return oapi.UploadSnapshotImportChunk400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}

	err := s.SnapshotTransfer.UploadImportChunk(ctx, request.SessionId, request.ChunkIndex, request.Body)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrSessionNotFound):
			return oapi.UploadSnapshotImportChunk404JSONResponse{Code: "not_found", Message: "snapshot import session not found"}, nil
		case errors.Is(err, snapshottransfer.ErrInvalidRequest):
			return oapi.UploadSnapshotImportChunk400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		case errors.Is(err, snapshottransfer.ErrConflict):
			return oapi.UploadSnapshotImportChunk409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to upload snapshot import chunk", "error", err, "session_id", request.SessionId, "chunk_index", request.ChunkIndex)
			return oapi.UploadSnapshotImportChunk500JSONResponse{Code: "internal_error", Message: "failed to upload snapshot import chunk"}, nil
		}
	}
	return oapi.UploadSnapshotImportChunk204Response{}, nil
}

func (s *ApiService) CompleteSnapshotImportSession(ctx context.Context, request oapi.CompleteSnapshotImportSessionRequestObject) (oapi.CompleteSnapshotImportSessionResponseObject, error) {
	snap, err := s.SnapshotTransfer.CompleteImportSession(ctx, request.SessionId)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrSessionNotFound):
			return oapi.CompleteSnapshotImportSession404JSONResponse{Code: "not_found", Message: "snapshot import session not found"}, nil
		case errors.Is(err, snapshottransfer.ErrConflict):
			return oapi.CompleteSnapshotImportSession409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to complete snapshot import session", "error", err, "session_id", request.SessionId)
			return oapi.CompleteSnapshotImportSession500JSONResponse{Code: "internal_error", Message: "failed to complete snapshot import session"}, nil
		}
	}
	return oapi.CompleteSnapshotImportSession201JSONResponse(snapshotToOAPI(instances.Snapshot(*snap))), nil
}

func (s *ApiService) CancelSnapshotImportSession(ctx context.Context, request oapi.CancelSnapshotImportSessionRequestObject) (oapi.CancelSnapshotImportSessionResponseObject, error) {
	err := s.SnapshotTransfer.CancelImportSession(ctx, request.SessionId)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, snapshottransfer.ErrSessionNotFound):
			return oapi.CancelSnapshotImportSession404JSONResponse{Code: "not_found", Message: "snapshot import session not found"}, nil
		case errors.Is(err, snapshottransfer.ErrConflict):
			return oapi.CancelSnapshotImportSession409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to cancel snapshot import session", "error", err, "session_id", request.SessionId)
			return oapi.CancelSnapshotImportSession500JSONResponse{Code: "internal_error", Message: "failed to cancel snapshot import session"}, nil
		}
	}
	return oapi.CancelSnapshotImportSession204Response{}, nil
}

func toOAPISnapshotTransfer(job snapshottransfer.TransferJob) oapi.SnapshotTransfer {
	status := oapi.SnapshotTransferStatus(job.Status)
	return oapi.SnapshotTransfer{
		Id:                   job.ID,
		SnapshotId:           job.SnapshotID,
		DestinationUrl:       job.DestinationURL,
		DestinationSessionId: job.DestinationSessionID,
		Status:               status,
		Error:                job.Error,
		CreatedAt:            job.CreatedAt,
		StartedAt:            job.StartedAt,
		CompletedAt:          job.CompletedAt,
		ChunksTotal:          job.ChunksTotal,
		ChunksTransferred:    job.ChunksTransferred,
		BytesTransferred:     job.BytesTransferred,
		DataSize:             job.DataSize,
	}
}

func toOAPISnapshotImportSession(session snapshottransfer.ImportSession) oapi.SnapshotImportSession {
	status := oapi.SnapshotImportSessionStatus(session.Status)
	return oapi.SnapshotImportSession{
		Id:                 session.ID,
		SourceSnapshotId:   session.SourceSnapshotID,
		Status:             status,
		CommittedChunkIds:  session.CommittedChunkIDs,
		ChunksTotal:        session.ChunksTotal,
		DataSize:           session.DataSize,
		ImportedSnapshotId: session.ImportedSnapshotID,
		CreatedAt:          session.CreatedAt,
		UpdatedAt:          session.UpdatedAt,
	}
}

func fromOAPISnapshotDescriptor(d oapi.SnapshotDescriptor) snapshottransfer.SnapshotDescriptor {
	name := ""
	if d.Name != nil {
		name = *d.Name
	}
	return snapshottransfer.SnapshotDescriptor{
		SourceSnapshotID:   d.SourceSnapshotId,
		SourceInstanceID:   d.SourceInstanceId,
		SourceInstanceName: d.SourceInstanceName,
		Name:               name,
		Kind:               snapshotstore.SnapshotKind(d.Kind),
		SourceHypervisor:   hypervisor.Type(d.SourceHypervisor),
		Tags:               toMapTags(d.Metadata),
		CreatedAt:          d.CreatedAt,
		SizeBytes:          d.SizeBytes,
		Compat: snapshottransfer.SourceSnapshotCompat{
			KernelVersion:     stringValue(d.Compat.KernelVersion),
			HypervisorVersion: stringValue(d.Compat.HypervisorVersion),
			Hypervisor:        hypervisor.Type(d.Compat.Hypervisor),
			PlatformOS:        d.Compat.PlatformOs,
			PlatformArch:      d.Compat.PlatformArch,
		},
	}
}

func fromOAPIManifest(manifest oapi.SnapshotImportManifest) snapshottransfer.Manifest {
	entries := make([]snapshottransfer.ManifestEntry, 0, len(manifest.Entries))
	for i := range manifest.Entries {
		entry := manifest.Entries[i]
		ex := make([]snapshottransfer.DataExtent, 0)
		if entry.Extents != nil {
			ex = make([]snapshottransfer.DataExtent, 0, len(*entry.Extents))
			for _, extent := range *entry.Extents {
				ex = append(ex, snapshottransfer.DataExtent{
					FileOffset: extent.FileOffset,
					Length:     extent.Length,
					DataOffset: extent.DataOffset,
				})
			}
		}
		entries = append(entries, snapshottransfer.ManifestEntry{
			Path:       entry.Path,
			Type:       string(entry.Type),
			Mode:       uint32(entry.Mode),
			Size:       int64Value(entry.Size),
			LinkTarget: stringValue(entry.LinkTarget),
			Extents:    ex,
		})
	}
	chunks := make([]snapshottransfer.ChunkDescriptor, 0, len(manifest.Chunks))
	for i := range manifest.Chunks {
		chunk := manifest.Chunks[i]
		chunks = append(chunks, snapshottransfer.ChunkDescriptor{
			Index:  chunk.Index,
			Offset: chunk.Offset,
			Size:   chunk.Size,
			SHA256: chunk.Sha256,
		})
	}
	return snapshottransfer.Manifest{
		Version:   manifest.Version,
		ChunkSize: manifest.ChunkSize,
		DataSize:  manifest.DataSize,
		Entries:   entries,
		Chunks:    chunks,
	}
}

func int64Value(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
