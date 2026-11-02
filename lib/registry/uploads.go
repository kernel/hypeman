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

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// uploadPattern matches /v2/{name}/blobs/uploads/ and /v2/{name}/blobs/uploads/{id}.
var uploadPattern = regexp.MustCompile(`^/v2/(.+)/blobs/uploads/([0-9a-f]*)$`)

// chunkedUploads stores chunked blob uploads (POST, PATCH..., PUT ?digest=) in
// files beside the blob store, hashing as they arrive. The upstream registry keeps
// them in memory, which a multi-gigabyte layer cannot afford. Monolithic uploads
// (POST ?digest=) already stream and are left to it.
type chunkedUploads struct {
	dir   string
	store *BlobStore
	mu    sync.Mutex
	open  map[string]*chunkedUpload
}

type chunkedUpload struct {
	mu   sync.Mutex
	file *os.File
	hash hash.Hash
	size int64
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
		u.start(w, repo)
	case id != "" && (req.Method == http.MethodPatch || req.Method == http.MethodPut || req.Method == http.MethodDelete):
		u.mu.Lock()
		upload := u.open[id]
		u.mu.Unlock()
		if upload == nil {
			http.Error(w, "blob upload unknown", http.StatusNotFound)
			return true
		}
		upload.mu.Lock()
		defer upload.mu.Unlock()
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

func (u *chunkedUploads) start(w http.ResponseWriter, repo string) {
	var raw [16]byte
	rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	file, err := os.OpenFile(filepath.Join(u.dir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		http.Error(w, "start upload failed", http.StatusInternalServerError)
		return
	}
	u.mu.Lock()
	u.open[id] = &chunkedUpload{file: file, hash: sha256.New()}
	u.mu.Unlock()
	w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
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
	n, err := io.Copy(io.MultiWriter(upload.file, upload.hash), req.Body)
	upload.size += n
	if err != nil {
		u.remove(id, upload)
		http.Error(w, "write upload failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
	w.Header().Set("Range", "0-"+strconv.FormatInt(upload.size-1, 10))
	w.WriteHeader(http.StatusAccepted)
}

// commit writes the final chunk and installs the blob if it matches the digest.
// The upload ends either way.
func (u *chunkedUploads) commit(w http.ResponseWriter, req *http.Request, repo, id string, upload *chunkedUpload) {
	defer u.remove(id, upload)
	want, err := v1.NewHash(req.URL.Query().Get("digest"))
	if err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}
	if _, err = io.Copy(io.MultiWriter(upload.file, upload.hash), req.Body); err == nil {
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

func (u *chunkedUploads) remove(id string, upload *chunkedUpload) {
	u.mu.Lock()
	delete(u.open, id)
	u.mu.Unlock()
	upload.file.Close()
	os.Remove(upload.file.Name())
}
