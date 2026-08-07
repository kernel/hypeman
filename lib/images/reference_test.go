package images

import (
	"context"
	"errors"
	"testing"

	gcr "github.com/google/go-containerregistry/pkg/v1"
)

// fakeInspector is a hermetic ManifestInspector for exercising the resolve seams
// without a registry round-trip.
type fakeInspector struct {
	manifestDigest     string
	manifestErr        error
	gotManifestRequest gcr.Platform
	digestPlatform     Platform
	digestResolved     string
	digestErr          error
	gotDigestRequest   gcr.Platform
}

func (f *fakeInspector) inspectManifestWithPlatform(ctx context.Context, imageRef string, platform gcr.Platform) (string, error) {
	f.gotManifestRequest = platform
	return f.manifestDigest, f.manifestErr
}

func (f *fakeInspector) inspectDigestPlatform(ctx context.Context, imageRef string, requested gcr.Platform) (Platform, string, error) {
	f.gotDigestRequest = requested
	if f.digestErr != nil {
		return Platform{}, "", f.digestErr
	}
	return f.digestPlatform, f.digestResolved, nil
}

func TestResolvedRefDigestRef(t *testing.T) {
	normalized, err := ParseNormalizedRef("docker.io/library/alpine:latest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ref := NewResolvedRef(normalized, "sha256:abc123")
	if got, want := ref.DigestRef(), "docker.io/library/alpine@sha256:abc123"; got != want {
		t.Fatalf("DigestRef() = %q, want %q", got, want)
	}
}

func TestResolveForPlatform(t *testing.T) {
	normalized, err := ParseNormalizedRef("docker.io/library/alpine:latest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fake := &fakeInspector{manifestDigest: "sha256:abc123"}
	requested := gcr.Platform{OS: "linux", Architecture: "amd64"}
	ref, err := normalized.ResolveForPlatform(context.Background(), fake, requested)
	if err != nil {
		t.Fatalf("ResolveForPlatform: %v", err)
	}
	if ref.Digest() != fake.manifestDigest {
		t.Fatalf("ref digest = %q, want %q", ref.Digest(), fake.manifestDigest)
	}
	got := fake.gotManifestRequest
	if got.OS != requested.OS || got.Architecture != requested.Architecture || got.Variant != requested.Variant {
		t.Fatalf("requested platform = %+v, want %+v", got, requested)
	}
}

func TestResolveDigest_RecordsResolvedChildDigest(t *testing.T) {
	const indexDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const childDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	normalized, err := ParseNormalizedRef("docker.io/library/alpine@" + indexDigest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fake := &fakeInspector{
		digestPlatform: Platform{OS: "linux", Architecture: "arm64"},
		digestResolved: childDigest,
	}

	actual, ref, err := normalized.ResolveDigest(context.Background(), fake, gcr.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}

	// The requested platform must be threaded through so an index pin selects the
	// right child instead of defaulting to amd64.
	if fake.gotDigestRequest.Architecture != "arm64" {
		t.Fatalf("requested platform not forwarded: got %+v", fake.gotDigestRequest)
	}
	if actual.Architecture != "arm64" {
		t.Fatalf("resolved platform = %s, want arm64", actual)
	}
	// The ref must carry the resolved CHILD digest (so an index pin dedups against
	// the same digest as the tag pull), not the originally-pinned index digest.
	if ref.Digest() != childDigest {
		t.Fatalf("ref digest = %s, want resolved child %s", ref.Digest(), childDigest)
	}
}

func TestResolveDigest_PropagatesNotFound(t *testing.T) {
	normalized, err := ParseNormalizedRef("docker.io/library/alpine@sha256:3333333333333333333333333333333333333333333333333333333333333333")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fake := &fakeInspector{digestErr: ErrNotFound}

	_, _, err = normalized.ResolveDigest(context.Background(), fake, gcr.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
