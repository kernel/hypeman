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
	digestPlatform   Platform
	digestResolved   string
	digestErr        error
	gotDigestRequest gcr.Platform
}

func (f *fakeInspector) inspectManifest(ctx context.Context, imageRef string) (string, error) {
	return "", errors.New("not implemented")
}

func (f *fakeInspector) inspectManifestWithPlatform(ctx context.Context, imageRef string, platform gcr.Platform) (string, error) {
	return "", errors.New("not implemented")
}

func (f *fakeInspector) inspectDigestPlatform(ctx context.Context, imageRef string, requested gcr.Platform) (Platform, string, error) {
	f.gotDigestRequest = requested
	if f.digestErr != nil {
		return Platform{}, "", f.digestErr
	}
	return f.digestPlatform, f.digestResolved, nil
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
