package api

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/images"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/scopes"
)

// maxConcurrentMachineBuildUploads bounds how many machine build bodies, each up
// to 65 MiB, are read into memory at once.
const maxConcurrentMachineBuildUploads = 4

// LimitMachineBuildBody must wrap the validator, which also consumes request bodies.
// A build upload is authenticated before it takes an upload slot, so unauthenticated
// requests cannot hold the slots.
func LimitMachineBuildBody(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		uploads := make(chan struct{}, maxConcurrentMachineBuildUploads)
		upload := mw.JwtAuth(jwtSecret)(scopes.RequireScope(scopes.BuildWrite)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case uploads <- struct{}{}:
				defer func() { <-uploads }()
			default:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"code":"too_many_requests","message":"too many machine build uploads in progress"}`))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 65<<20)
			next.ServeHTTP(w, r)
		})))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/builds" {
				upload.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Machine-only HTTP admission is bounded regardless of multipart field order.
// Linux retains its existing parser and source contract.
func parseMachineBuildMultipart(body *multipart.Reader) (builds.CreateBuildRequest, []byte, error) {
	req := builds.CreateBuildRequest{BuildPolicy: &builds.BuildPolicy{}}
	var source []byte
	seen := map[string]bool{}
	if body == nil {
		return req, nil, fmt.Errorf("multipart source is required")
	}
	for {
		part, err := body.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return req, nil, fmt.Errorf("invalid multipart form")
		}
		name := part.FormName()
		if seen[name] {
			return req, nil, fmt.Errorf("duplicate multipart field")
		}
		seen[name] = true
		limit := int64(64 << 10)
		switch name {
		case "source":
			limit = 64 << 20
		case "machine_base_image", "timeout_seconds", "memory_mb", "cpus", "tags":
		default:
			return req, nil, fmt.Errorf("unsupported machine build field: %s", name)
		}
		data, err := io.ReadAll(io.LimitReader(part, limit+1))
		if err != nil || int64(len(data)) > limit {
			return req, nil, fmt.Errorf("invalid or oversized %s field", name)
		}
		if err = part.Close(); err != nil {
			return req, nil, fmt.Errorf("invalid multipart field")
		}
		switch name {
		case "source":
			source = data
		case "machine_base_image":
			req.MachineBaseImage = string(data)
		case "tags":
			req.Tags, err = parseTagsJSON(string(data))
			if err != nil {
				return req, nil, fmt.Errorf("tags must be a JSON object of strings")
			}
		default:
			value, err := strconv.Atoi(string(data))
			if err != nil || value < 0 {
				return req, nil, fmt.Errorf("%s must be a nonnegative integer", name)
			}
			switch name {
			case "timeout_seconds":
				req.BuildPolicy.TimeoutSeconds = value
			case "memory_mb":
				req.BuildPolicy.MemoryMB = value
			case "cpus":
				req.BuildPolicy.CPUs = value
			}
		}
	}
	if len(source) == 0 {
		return req, nil, fmt.Errorf("source is required")
	}
	ref, err := images.ParseNormalizedRef(req.MachineBaseImage)
	if err != nil || !ref.IsDigest() || !builds.ValidMachineDigest(ref.Digest()) {
		return req, nil, fmt.Errorf("machine_base_image must be pinned by sha256 digest")
	}
	if req.BuildPolicy.TimeoutSeconds > 86400 {
		return req, nil, fmt.Errorf("timeout_seconds exceeds 86400")
	}
	if err = req.BuildPolicy.Validate(); err != nil {
		return req, nil, err
	}
	return req, source, nil
}
