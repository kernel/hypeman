package images

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseNormalizedRef(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		wantErr  bool
	}{
		// Valid images with full reference
		{"docker.io/library/alpine:latest", "docker.io/library/alpine:latest", false},
		{"ghcr.io/myorg/myapp:v1.0.0", "ghcr.io/myorg/myapp:v1.0.0", false},

		// Shorthand (gets expanded)
		{"alpine", "docker.io/library/alpine:latest", false},
		{"alpine:3.18", "docker.io/library/alpine:3.18", false},
		{"nginx", "docker.io/library/nginx:latest", false},
		{"nginx:alpine", "docker.io/library/nginx:alpine", false},

		// Without tag (gets :latest added)
		{"docker.io/library/alpine", "docker.io/library/alpine:latest", false},
		{"ubuntu", "docker.io/library/ubuntu:latest", false},

		// Digest references (must be valid 64-char hex SHA256)
		{"alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "docker.io/library/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"docker.io/library/alpine@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", "docker.io/library/alpine@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", false},

		// Invalid
		{"", "", true},
		{"invalid::", "", true},
		{"has spaces", "", true},
		{"UPPERCASE", "", true}, // Repository names must be lowercase
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := ParseNormalizedRef(tt.input)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected, result.String())
			}
		})
	}
}

func TestNormalizedRefMethods(t *testing.T) {
	t.Run("TaggedReference", func(t *testing.T) {
		ref, err := ParseNormalizedRef("alpine:3.18")
		require.NoError(t, err)

		require.False(t, ref.IsDigest())

		repo := ref.Repository()
		require.Equal(t, "docker.io/library/alpine", repo)

		tag := ref.Tag()
		require.Equal(t, "3.18", tag)

		digest := ref.Digest()
		require.Equal(t, "", digest)

		digestHex := ref.DigestHex()
		require.Equal(t, "", digestHex)
	})

	t.Run("DigestReference", func(t *testing.T) {
		ref, err := ParseNormalizedRef("alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		require.NoError(t, err)

		require.True(t, ref.IsDigest())

		repo := ref.Repository()
		require.Equal(t, "docker.io/library/alpine", repo)

		tag := ref.Tag()
		require.Equal(t, "", tag)

		digest := ref.Digest()
		require.Equal(t, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", digest)

		digestHex := ref.DigestHex()
		require.Equal(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", digestHex)
	})

	t.Run("DefaultTag", func(t *testing.T) {
		ref, err := ParseNormalizedRef("alpine")
		require.NoError(t, err)

		tag := ref.Tag()
		require.Equal(t, "latest", tag)
	})
}
