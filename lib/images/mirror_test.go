package images

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeToLocalRef(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "docker hub user image with tag",
			input:    "docker.io/onkernel/nodejs22-base:0.1.1",
			expected: "onkernel/nodejs22-base:0.1.1",
		},
		{
			name:     "docker hub user image without registry prefix",
			input:    "onkernel/nodejs22-base:0.1.1",
			expected: "onkernel/nodejs22-base:0.1.1",
		},
		{
			name:     "docker hub official image with tag",
			input:    "docker.io/library/alpine:3.21",
			expected: "library/alpine:3.21",
		},
		{
			name:     "docker hub official image short form",
			input:    "alpine:3.21",
			expected: "library/alpine:3.21",
		},
		{
			name:     "docker hub image with index.docker.io",
			input:    "index.docker.io/onkernel/nodejs22-base:0.1.1",
			expected: "onkernel/nodejs22-base:0.1.1",
		},
		{
			name:     "gcr.io image",
			input:    "gcr.io/google-containers/pause:3.2",
			expected: "gcr.io/google-containers/pause:3.2",
		},
		{
			name:     "ghcr.io image",
			input:    "ghcr.io/some-org/some-image:v1.0",
			expected: "ghcr.io/some-org/some-image:v1.0",
		},
		{
			name:     "image with latest tag",
			input:    "nginx:latest",
			expected: "library/nginx:latest",
		},
		{
			name:     "image without tag uses latest",
			input:    "nginx",
			expected: "library/nginx:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := name.ParseReference(tt.input)
			require.NoError(t, err)
			result := normalizeToLocalRef(ref)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStripScheme(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://localhost:8080", "localhost:8080"},
		{"http://localhost:8080", "localhost:8080"},
		{"localhost:8080", "localhost:8080"},
		{"https://registry.example.com", "registry.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := stripScheme(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
