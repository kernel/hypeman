package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// uploadPattern matches /v2/{name}/blobs/uploads/ and /v2/{name}/blobs/uploads/{id}.
var uploadPattern = regexp.MustCompile(`^/v2/(.+)/blobs/uploads/([0-9a-f]*)$`)

// Bounds on chunked uploads, which otherwise hold host disk until they end or the
// process restarts. One upload fits the largest machine image layer.
const (
	maxUploadBytes     = 128 << 30
	maxOpenUploadBytes = 256 << 30
	maxOpenUploads     = 64
	uploadIdleTimeout  = time.Hour
)

// chunkedUploads stores chunked blob uploads (POST, PATCH..., PUT ?digest=) in
// files beside the blob store, hashing as they arrive. The upstream registry keeps
// them in memory, which a multi-gigabyte layer cannot afford. Monolithic uploads
// (POST ?digest=) already stream and are left to it.
type chunkedUploads struct {
	dir       string
	store     *BlobStore
	mu        sync.Mutex
	open      map[string]*chunkedUpload
	openBytes atomic.Int64
}

type chunkedUpload struct {
	mu       sync.Mutex
	file     *os.File
	hash     hash.Hash
	size     int64
	lastUsed time.Time
}

// newChunkedUploads removes uploads left by an earlier process: their sessions
// were in memory, so no client can finish them.
func newChunkedUploads(dir string, store *BlobStore) (*chunkedUploads, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &chunkedUploads{dir: dir, store: store, open: make(map[string]*chunkedUpload)}, nil
}

// serve handles req if it is part of a chunked upload and reports whether it did.
func (u *chunkedUploads) serve(w http.ResponseWriter, req *http.Request) bool {
	match := uploadPattern.FindStringSubmatch(req.URL.Path)
	if match == nil {
		return false
	}
	repo, id := match[1], match[2]
	switch {
	case req.Method == http.MethodPost && id == "" && req.URL.Query().Get("digest") == "":
		u.removeIdle()
		u.start(w, repo)
	case id != "" && (req.Method == http.MethodPatch || req.Method == http.MethodPut || req.Method == http.MethodDelete):
		upload := u.lock(id)
		if upload == nil {
			http.Error(w, "blob upload unknown", http.StatusNotFound)
			return true
		}
		defer upload.mu.Unlock()
		upload.lastUsed = time.Now()
		switch req.Method {
		case http.MethodPatch:
			u.append(w, req, repo, id, upload)
		case http.MethodPut:
			u.commit(w, req, repo, id, upload)
		default:
			u.remove(id, upload)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		return false
	}
	return true
}

// lock returns the open upload id, locked, or nil. An upload ended while this
// request waited for it is not returned.
func (u *chunkedUploads) lock(id string) *chunkedUpload {
	u.mu.Lock()
	upload := u.open[id]
	u.mu.Unlock()
	if upload == nil {
		return nil
	}
	upload.mu.Lock()
	u.mu.Lock()
	current := u.open[id] == upload
	u.mu.Unlock()
	if !current {
		upload.mu.Unlock()
		return nil
	}
	return upload
}

// removeIdle ends uploads unused for uploadIdleTimeout. An upload busy with a
// request is not idle.
func (u *chunkedUploads) removeIdle() {
	u.mu.Lock()
	idle := make(map[string]*chunkedUpload)
	for id, upload := range u.open {
		if upload.mu.TryLock() {
			if time.Since(upload.lastUsed) > uploadIdleTimeout {
				idle[id] = upload
			} else {
				upload.mu.Unlock()
			}
		}
	}
	u.mu.Unlock()
	for id, upload := range idle {
		u.remove(id, upload)
		upload.mu.Unlock()
	}
}

func (u *chunkedUploads) start(w http.ResponseWriter, repo string) {
	u.mu.Lock()
	full := len(u.open) >= maxOpenUploads
	u.mu.Unlock()
	if full {
		http.Error(w, "too many open blob uploads", http.StatusTooManyRequests)
		return
	}
	var raw [16]byte
	rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	file, err := os.OpenFile(filepath.Join(u.dir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		http.Error(w, "start upload failed", http.StatusInternalServerError)
		return
	}
	u.mu.Lock()
	u.open[id] = &chunkedUpload{file: file, hash: sha256.New(), lastUsed: time.Now()}
	u.mu.Unlock()
	w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
}

// write appends body to upload, within the size bounds. It reports whether the
// body fit; an upload that does not fit is ended.
func (u *chunkedUploads) write(w http.ResponseWriter, id string, upload *chunkedUpload, body io.Reader) bool {
	allowed := min(maxUploadBytes-upload.size, maxOpenUploadBytes-u.openBytes.Load())
	n, err := io.Copy(io.MultiWriter(upload.file, upload.hash), io.LimitReader(body, max(allowed, 0)+1))
	upload.size += n
	u.openBytes.Add(n)
	switch {
	case err != nil:
		u.remove(id, upload)
		http.Error(w, "write upload failed", http.StatusInternalServerError)
		return false
	case n > allowed:
		u.remove(id, upload)
		http.Error(w, "blob upload too large", http.StatusRequestEntityTooLarge)
		return false
	}
	return true
}

// append writes a chunk. A Content-Range must start where the upload ends.
func (u *chunkedUploads) append(w http.ResponseWriter, req *http.Request, repo, id string, upload *chunkedUpload) {
	if r := req.Header.Get("Content-Range"); r != "" {
		var start, end int64
		if _, err := fmt.Sscanf(r, "%d-%d", &start, &end); err != nil || start != upload.size {
			http.Error(w, "content range does not continue the upload", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	if !u.write(w, id, upload, req.Body) {
		return
	}
	w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
	if upload.size > 0 {
		w.Header().Set("Range", "0-"+strconv.FormatInt(upload.size-1, 10))
	}
	w.WriteHeader(http.StatusAccepted)
}

// commit writes the final chunk and installs the blob if it matches the digest.
// The upload ends either way.
func (u *chunkedUploads) commit(w http.ResponseWriter, req *http.Request, repo, id string, upload *chunkedUpload) {
	want, err := v1.NewHash(req.URL.Query().Get("digest"))
	if err != nil {
		u.remove(id, upload)
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}
	if !u.write(w, id, upload, req.Body) {
		return
	}
	defer u.remove(id, upload)
	if err = upload.file.Sync(); err == nil {
		err = upload.file.Close()
	}
	if err != nil {
		http.Error(w, "write upload failed", http.StatusInternalServerError)
		return
	}
	if got := "sha256:" + hex.EncodeToString(upload.hash.Sum(nil)); got != want.String() {
		http.Error(w, "digest mismatch", http.StatusBadRequest)
		return
	}
	if err = u.store.install(upload.file.Name(), want); err != nil {
		http.Error(w, "store blob failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Docker-Content-Digest", want.String())
	w.Header().Set("Location", "/v2/"+repo+"/blobs/"+want.String())
	w.WriteHeader(http.StatusCreated)
}

// remove ends an upload and deletes its file. Ending an ended upload does nothing.
func (u *chunkedUploads) remove(id string, upload *chunkedUpload) {
	u.mu.Lock()
	current := u.open[id] == upload
	if current {
		delete(u.open, id)
	}
	u.mu.Unlock()
	if !current {
		return
	}
	u.openBytes.Add(-upload.size)
	upload.file.Close()
	os.Remove(upload.file.Name())
}
