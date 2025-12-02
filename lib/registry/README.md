# OCI Distribution Registry

Implements an OCI Distribution Spec compliant registry that accepts pushed images and triggers conversion to hypeman's disk format.

## Architecture

```mermaid
sequenceDiagram
    participant Client as Docker Client
    participant Registry as Hypeman Registry
    participant BlobStore as Blob Store
    participant ImageMgr as Image Manager

    Client->>Registry: PUT /v2/.../blobs/{digest}
    Registry->>BlobStore: Store blob
    BlobStore-->>Registry: OK
    Registry-->>Client: 201 Created

    Client->>Registry: PUT /v2/.../manifests/{ref}
    Registry->>BlobStore: Store manifest blob
    Registry->>Registry: Update index.json
    Registry-->>Client: 201 Created
    
    Registry--)ImageMgr: ImportLocalImage() (async)
    ImageMgr->>ImageMgr: Queue conversion
    ImageMgr->>ImageMgr: Unpack layers
    ImageMgr->>ImageMgr: Create disk image
```

## How It Works

### Push Flow

1. **Version Check**: Client hits `GET /v2/` to verify registry compatibility
2. **Blob Check**: Client does `HEAD /v2/{name}/blobs/{digest}` to check if layers exist
3. **Blob Upload**: Missing blobs uploaded via `POST/PATCH/PUT` sequence
4. **Manifest Upload**: Final `PUT /v2/{name}/manifests/{reference}` triggers conversion

### Layer Caching

Blobs are stored content-addressably in `system/oci-cache/blobs/sha256/`:

```go
// BlobStore.Stat() - Returns size if exists, ErrNotFound otherwise
func (s *BlobStore) Stat(ctx context.Context, repo string, h v1.Hash) (int64, error) {
    path := s.blobPath(h.String())
    info, err := os.Stat(path)
    if os.IsNotExist(err) {
        return 0, ErrNotFound  // Client will upload
    }
    return info.Size(), nil    // Client skips upload
}
```

When a client pushes:
- First push: HEAD returns 404 → uploads all blobs
- Second push: HEAD returns 200 with size → skips upload entirely

### Manifest Handling

go-containerregistry stores manifests in-memory, but we need them on disk for conversion. The registry intercepts manifest PUTs:

```go
// Read manifest body before passing to underlying handler
body, _ := io.ReadAll(req.Body)

// Store in blob store (for image manager to read later)
r.storeManifestBlob(reference, body)

// Reconstruct body for underlying handler
req.Body = io.NopCloser(bytes.NewReader(body))
r.handler.ServeHTTP(wrapper, req)

// Trigger async conversion
if wrapper.statusCode == http.StatusCreated {
    go r.triggerConversion(repo, reference)
}
```

### Conversion Trigger

After a successful manifest push:

1. Updates `index.json` with manifest entry (correct mediaType detected from content)
2. Calls `ImageManager.ImportLocalImage()` to queue conversion
3. Image manager reads from shared OCI cache and creates disk image

## Files

- **`blob_store.go`** - Filesystem-backed blob storage implementing `registry.BlobHandler`
- **`registry.go`** - Registry handler wrapping go-containerregistry with manifest interception

## Storage Layout

```
/var/lib/hypeman/system/oci-cache/
  oci-layout           # {"imageLayoutVersion": "1.0.0"}
  index.json           # Manifest index with annotations
  blobs/sha256/
    2d35eb...          # Layer blob (shared across all images)
    706db5...          # Config blob
    85f2b7...          # Manifest blob
```

## CLI Usage

```bash
# Push from local Docker daemon
hypeman push myimage:latest

# Push with custom target name
hypeman push myimage:latest my-custom-name
```

## Authentication

The registry endpoints use the same JWT authentication as the rest of the API. The CLI reads `HYPEMAN_API_KEY` or `HYPEMAN_BEARER_TOKEN` and passes it as a registry token.

## Limitations

- **Docker v2 manifests**: Images from local Docker daemon use Docker v2 manifest format which may need additional handling for conversion. Images pulled directly from registries (OCI format) work seamlessly.
- **Digest references only**: Conversion is triggered only for digest references (`@sha256:...`), not tag references (`:latest`). The CLI automatically converts to digest references.

## Design Decisions

### Why wrap go-containerregistry/pkg/registry?

**What:** Use the existing registry implementation from go-containerregistry with custom blob storage.

**Why:**
- Battle-tested OCI Distribution Spec compliance
- Handles chunked uploads, content negotiation, error responses
- We only need to customize storage, not protocol handling

### Why store manifests separately?

**What:** Intercept manifest PUT and store in blob store.

**Why:**
- go-containerregistry stores manifests in-memory by default
- Our image manager needs to read manifests from disk
- Enables content-addressable manifest storage consistent with layers

### Why detect mediaType from manifest content?

**What:** Parse manifest JSON to extract actual mediaType rather than hardcoding.

**Why:**
- Docker v2 (`application/vnd.docker.distribution.manifest.v2+json`) vs OCI (`application/vnd.oci.image.manifest.v1+json`)
- Wrong mediaType in index.json causes "malicious manifest" errors during conversion
- Content-based detection ensures compatibility with all sources
