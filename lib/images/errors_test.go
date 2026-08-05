package images

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassifyRegistryError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want error // nil means "returned unchanged / not classified"
	}{
		{name: "nil stays nil", in: nil, want: nil},
		{
			name: "no child with platform -> platform not available",
			in:   errors.New("fetch manifest: no child with platform linux/amd64 in index docker.io/arm64v8/alpine:3.19"),
			want: ErrPlatformNotAvailable,
		},
		{
			name: "absent variant -> platform not available",
			in:   errors.New("no child with platform linux/amd64/v2 in index"),
			want: ErrPlatformNotAvailable,
		},
		{
			name: "TOOMANYREQUESTS -> rate limited",
			in:   errors.New("GET https://registry-1.docker.io/v2/: TOOMANYREQUESTS: You have reached your unauthenticated pull rate limit"),
			want: ErrRateLimited,
		},
		{
			name: "too many requests phrasing -> rate limited",
			in:   errors.New("429 Too Many Requests"),
			want: ErrRateLimited,
		},
		{
			name: "NAME_UNKNOWN -> not found",
			in:   errors.New("NAME_UNKNOWN: repository name not known to registry"),
			want: ErrNotFound,
		},
		{
			name: "MANIFEST_UNKNOWN -> not found",
			in:   errors.New("MANIFEST_UNKNOWN: manifest unknown"),
			want: ErrNotFound,
		},
		{
			name: "UNAUTHORIZED bogus repo -> not found",
			in:   errors.New("UNAUTHORIZED: authentication required"),
			want: ErrNotFound,
		},
		{
			name: "404 -> not found",
			in:   errors.New("GET https://example/v2/x: 404 Not Found"),
			want: ErrNotFound,
		},
		{
			name: "unrecognized error returned unchanged",
			in:   errors.New("connection refused"),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRegistryError(tt.in)
			if tt.in == nil {
				if got != nil {
					t.Fatalf("ClassifyRegistryError(nil) = %v, want nil", got)
				}
				return
			}
			if tt.want == nil {
				// Should be returned unchanged and not match any typed error.
				if got != tt.in {
					t.Fatalf("ClassifyRegistryError(%v) = %v, want unchanged", tt.in, got)
				}
				for _, sentinel := range []error{ErrPlatformNotAvailable, ErrRateLimited, ErrNotFound} {
					if errors.Is(got, sentinel) {
						t.Fatalf("ClassifyRegistryError(%v) unexpectedly classified as %v", tt.in, sentinel)
					}
				}
				return
			}
			if !errors.Is(got, tt.want) {
				t.Fatalf("ClassifyRegistryError(%v) = %v, want errors.Is %v", tt.in, got, tt.want)
			}
			// The underlying registry message must be preserved for operators.
			if !strings.Contains(got.Error(), tt.in.Error()) {
				t.Fatalf("ClassifyRegistryError(%v) dropped the cause text: %v", tt.in, got)
			}
		})
	}
}

// TestClassifyRegistryErrorPrecedence asserts rate-limit classification wins over a
// coincidental "404"/"not found" substring so a throttled pull is retryable
// rather than reported as a permanent not-found.
func TestClassifyRegistryErrorPrecedence(t *testing.T) {
	err := ClassifyRegistryError(fmt.Errorf("429 too many requests (not found in cache)"))
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want ErrRateLimited", err)
	}
}
