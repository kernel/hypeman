package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRegistryServer creates a test server that responds to manifest HEAD requests.
// The availableImages map determines which images return 200 (found) vs 404 (not found).
func mockRegistryServer(availableImages map[string]bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse the path to extract repo and tag
		// Expected format: /v2/{repo}/manifests/{tag}
		path := r.URL.Path
		if !strings.HasPrefix(path, "/v2/") || !strings.Contains(path, "/manifests/") {
			http.NotFound(w, r)
			return
		}

		// Extract repo and tag
		parts := strings.SplitN(strings.TrimPrefix(path, "/v2/"), "/manifests/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}

		imageRef := parts[0] + ":" + parts[1]

		if availableImages[imageRef] {
			w.WriteHeader(http.StatusOK)
		} else {
			http.NotFound(w, r)
		}
	}))
}

func TestRewriteDockerfileFROMs(t *testing.T) {
	// Create a mock registry with specific images available
	availableImages := map[string]bool{
		"onkernel/nodejs22-base:0.1.1": true,
		"onkernel/python311-base:0.1.1": true,
	}
	server := mockRegistryServer(availableImages)
	defer server.Close()

	// Extract host from server URL (remove http://)
	registryURL := strings.TrimPrefix(server.URL, "http://")

	tests := []struct {
		name          string
		dockerfile    string
		expectedCount int
		expected      string
	}{
		{
			name: "simple FROM rewrite when image exists locally",
			dockerfile: `FROM onkernel/nodejs22-base:0.1.1
RUN echo hello`,
			expectedCount: 1,
			expected: `FROM ` + registryURL + `/onkernel/nodejs22-base:0.1.1
RUN echo hello`,
		},
		{
			name: "FROM with docker.io prefix",
			dockerfile: `FROM docker.io/onkernel/nodejs22-base:0.1.1
RUN echo hello`,
			expectedCount: 1,
			expected: `FROM ` + registryURL + `/onkernel/nodejs22-base:0.1.1
RUN echo hello`,
		},
		{
			name: "FROM with AS alias",
			dockerfile: `FROM onkernel/nodejs22-base:0.1.1 AS builder
RUN npm install
FROM onkernel/nodejs22-base:0.1.1 AS runtime
COPY --from=builder /app /app`,
			expectedCount: 2,
			expected: `FROM ` + registryURL + `/onkernel/nodejs22-base:0.1.1 AS builder
RUN npm install
FROM ` + registryURL + `/onkernel/nodejs22-base:0.1.1 AS runtime
COPY --from=builder /app /app`,
		},
		{
			name: "FROM with --platform flag",
			dockerfile: `FROM --platform=linux/amd64 onkernel/nodejs22-base:0.1.1
RUN echo hello`,
			expectedCount: 1,
			expected: `FROM --platform=linux/amd64 ` + registryURL + `/onkernel/nodejs22-base:0.1.1
RUN echo hello`,
		},
		{
			name: "no rewrite when image not in local registry",
			dockerfile: `FROM alpine:3.21
RUN echo hello`,
			expectedCount: 0,
			expected: `FROM alpine:3.21
RUN echo hello`,
		},
		{
			name: "preserves comments and whitespace",
			dockerfile: `# This is a comment
FROM onkernel/nodejs22-base:0.1.1

# Another comment
RUN echo hello`,
			expectedCount: 1,
			expected: `# This is a comment
FROM ` + registryURL + `/onkernel/nodejs22-base:0.1.1

# Another comment
RUN echo hello`,
		},
		{
			name: "lowercase from is rewritten",
			dockerfile: `from onkernel/nodejs22-base:0.1.1
RUN echo hello`,
			expectedCount: 1,
			expected: `from ` + registryURL + `/onkernel/nodejs22-base:0.1.1
RUN echo hello`,
		},
		{
			name: "scratch image is not rewritten",
			dockerfile: `FROM scratch
COPY binary /`,
			expectedCount: 0,
			expected: `FROM scratch
COPY binary /`,
		},
		{
			name: "already local registry reference is not rewritten",
			dockerfile: `FROM ` + registryURL + `/onkernel/nodejs22-base:0.1.1
RUN echo hello`,
			expectedCount: 0,
			expected: `FROM ` + registryURL + `/onkernel/nodejs22-base:0.1.1
RUN echo hello`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpDir := t.TempDir()
			dockerfilePath := filepath.Join(tmpDir, "Dockerfile")
			err := os.WriteFile(dockerfilePath, []byte(tt.dockerfile), 0644)
			require.NoError(t, err)

			// Run rewrite (insecure=true for http test server)
			count, err := rewriteDockerfileFROMs(dockerfilePath, registryURL, true, "")
			require.NoError(t, err)
			assert.Equal(t, tt.expectedCount, count)

			// Check result
			result, err := os.ReadFile(dockerfilePath)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, string(result))
		})
	}
}

func TestNormalizeImageRef(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"docker.io/onkernel/nodejs22-base:0.1.1", "onkernel/nodejs22-base:0.1.1"},
		{"docker.io/library/alpine:3.21", "alpine:3.21"},
		{"onkernel/nodejs22-base:0.1.1", "onkernel/nodejs22-base:0.1.1"},
		{"alpine:3.21", "alpine:3.21"},
		{"library/alpine:3.21", "alpine:3.21"},
		{"nginx", "nginx"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizeImageRef(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCheckImageExistsInRegistry(t *testing.T) {
	availableImages := map[string]bool{
		"myimage:v1":          true,
		"org/myimage:latest":  true,
	}
	server := mockRegistryServer(availableImages)
	defer server.Close()

	registryURL := strings.TrimPrefix(server.URL, "http://")

	tests := []struct {
		name     string
		imageRef string
		expected bool
	}{
		{
			name:     "image exists with tag",
			imageRef: "myimage:v1",
			expected: true,
		},
		{
			name:     "image exists with org and latest tag",
			imageRef: "org/myimage:latest",
			expected: true,
		},
		{
			name:     "image does not exist",
			imageRef: "notfound:v1",
			expected: false,
		},
		{
			name:     "image without tag defaults to latest",
			imageRef: "org/myimage",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checkImageExistsInRegistry(registryURL, tt.imageRef, true, "")
			assert.Equal(t, tt.expected, result)
		})
	}
}
