package instances

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/kernel/hypeman/lib/images"
)

type createImageResolverFake struct {
	getImage    func(context.Context, string) (*images.Image, error)
	createImage func(context.Context, images.CreateImageRequest) (*images.Image, error)
	waitReady   func(context.Context, string) error
}

func (f createImageResolverFake) CreateImage(ctx context.Context, req images.CreateImageRequest) (*images.Image, error) {
	return f.createImage(ctx, req)
}

func (f createImageResolverFake) GetImage(ctx context.Context, name string) (*images.Image, error) {
	return f.getImage(ctx, name)
}

func (f createImageResolverFake) WaitForReady(ctx context.Context, name string) error {
	if f.waitReady == nil {
		return nil
	}
	return f.waitReady(ctx, name)
}

func TestResolveImageForCreateWithPlatformForcesPlatformResolve(t *testing.T) {
	t.Parallel()

	getCalled := false
	createCalled := false
	resolver := createImageResolverFake{
		getImage: func(context.Context, string) (*images.Image, error) {
			getCalled = true
			return &images.Image{
				Name:     "docker.io/library/alpine:3.19",
				Platform: "linux/arm64",
				Status:   images.StatusReady,
			}, nil
		},
		createImage: func(_ context.Context, req images.CreateImageRequest) (*images.Image, error) {
			createCalled = true
			if req.Platform != "linux/amd64" {
				t.Fatalf("expected platform linux/amd64, got %q", req.Platform)
			}
			return &images.Image{
				Name:     req.Name,
				Digest:   "sha256:amd64",
				Platform: "linux/amd64",
				Status:   images.StatusReady,
			}, nil
		},
	}

	img, err := resolveImageForCreate(context.Background(), resolver, "docker.io/library/alpine:3.19", "linux/amd64", slog.Default())
	if err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if !createCalled {
		t.Fatal("expected CreateImage to be called for explicit platform")
	}
	if getCalled {
		t.Fatal("explicit platform should not trust platform-blind tag lookup before resolving")
	}
	if img.Platform != "linux/amd64" {
		t.Fatalf("expected amd64 image, got %s", img.Platform)
	}
}

func TestResolveImageForCreateRejectsResolvedPlatformMismatch(t *testing.T) {
	t.Parallel()

	resolver := createImageResolverFake{
		getImage: func(context.Context, string) (*images.Image, error) {
			t.Fatal("GetImage should not be called for ready image returned by CreateImage")
			return nil, nil
		},
		createImage: func(_ context.Context, req images.CreateImageRequest) (*images.Image, error) {
			return &images.Image{
				Name:     req.Name,
				Digest:   "sha256:arm64",
				Platform: "linux/arm64",
				Status:   images.StatusReady,
			}, nil
		},
	}

	_, err := resolveImageForCreate(context.Background(), resolver, "docker.io/library/alpine:3.19", "linux/amd64", slog.Default())
	if !errors.Is(err, images.ErrInvalidPlatform) {
		t.Fatalf("expected invalid platform error, got %v", err)
	}
}

func TestResolveImageForCreateWithPlatformWaitsOnPinnedDigest(t *testing.T) {
	t.Parallel()

	const imageName = "docker.io/library/alpine:3.19"
	const pinnedName = "docker.io/library/alpine@sha256:amd64"

	waitName := ""
	resolver := createImageResolverFake{
		getImage: func(_ context.Context, name string) (*images.Image, error) {
			if name != pinnedName {
				t.Fatalf("expected pinned image lookup %q, got %q", pinnedName, name)
			}
			return &images.Image{
				Name:     imageName,
				Digest:   "sha256:amd64",
				Platform: "linux/amd64",
				Status:   images.StatusReady,
			}, nil
		},
		createImage: func(_ context.Context, req images.CreateImageRequest) (*images.Image, error) {
			return &images.Image{
				Name:     req.Name,
				Digest:   "sha256:amd64",
				Platform: "linux/amd64",
				Status:   images.StatusPending,
			}, nil
		},
		waitReady: func(_ context.Context, name string) error {
			waitName = name
			return nil
		},
	}

	img, err := resolveImageForCreate(context.Background(), resolver, imageName, "linux/amd64", slog.Default())
	if err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if waitName != pinnedName {
		t.Fatalf("expected WaitForReady on %q, got %q", pinnedName, waitName)
	}
	if img.Platform != "linux/amd64" {
		t.Fatalf("expected amd64 image, got %s", img.Platform)
	}
}

// A no-platform create that finds a host-native cached image returns it without
// a registry round-trip (covers locally cached host images and locally-built /
// platformless images, whose stored platform reports as host-native).
func TestResolveImageForCreateWithoutPlatformUsesHostCachedImage(t *testing.T) {
	t.Parallel()

	createCalled := false
	resolver := createImageResolverFake{
		getImage: func(context.Context, string) (*images.Image, error) {
			return &images.Image{
				Name:     "docker.io/library/alpine:3.19",
				Platform: images.HostPlatformString(),
				Status:   images.StatusReady,
			}, nil
		},
		createImage: func(context.Context, images.CreateImageRequest) (*images.Image, error) {
			createCalled = true
			return nil, nil
		},
	}

	img, err := resolveImageForCreate(context.Background(), resolver, "docker.io/library/alpine:3.19", "", slog.Default())
	if err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if createCalled {
		t.Fatal("default platform run should use host-cached image when present")
	}
	if img.Platform != images.HostPlatformString() {
		t.Fatalf("expected host image, got %s", img.Platform)
	}
}

// A no-platform create must NOT trust a tag pointer that resolves to a non-host
// arch (last-pull-wins can point the tag at an emulated variant). It must
// re-resolve the host variant explicitly and never silently emulate.
func TestResolveImageForCreateWithoutPlatformIgnoresNonHostTagPointer(t *testing.T) {
	t.Parallel()

	const imageName = "docker.io/library/alpine:3.19"
	nonHost := nonHostPlatformString()

	createReqPlatform := ""
	resolver := createImageResolverFake{
		getImage: func(_ context.Context, name string) (*images.Image, error) {
			// First call: tag lookup resolves the non-host (last-pull-wins) variant.
			// Subsequent call is the pinned-digest readiness lookup.
			if name == imageName {
				return &images.Image{
					Name:     imageName,
					Platform: nonHost,
					Status:   images.StatusReady,
				}, nil
			}
			return &images.Image{
				Name:     imageName,
				Digest:   "sha256:host",
				Platform: images.HostPlatformString(),
				Status:   images.StatusReady,
			}, nil
		},
		createImage: func(_ context.Context, req images.CreateImageRequest) (*images.Image, error) {
			createReqPlatform = req.Platform
			return &images.Image{
				Name:     req.Name,
				Digest:   "sha256:host",
				Platform: images.HostPlatformString(),
				Status:   images.StatusReady,
			}, nil
		},
	}

	img, err := resolveImageForCreate(context.Background(), resolver, imageName, "", slog.Default())
	if err != nil {
		t.Fatalf("resolve image: %v", err)
	}
	if createReqPlatform != images.HostPlatformString() {
		t.Fatalf("expected host platform resolve, got %q", createReqPlatform)
	}
	if img.Platform != images.HostPlatformString() {
		t.Fatalf("expected host image, got %s", img.Platform)
	}
}

// A no-platform create of an image with no host variant must surface a clear
// platform-not-available error rather than emulating, so the handler maps it to
// 404 platform_not_available telling the user to pass --platform.
func TestResolveImageForCreateWithoutPlatformHostVariantAbsent(t *testing.T) {
	t.Parallel()

	const imageName = "docker.io/library/alpine:3.19"
	resolver := createImageResolverFake{
		getImage: func(context.Context, string) (*images.Image, error) {
			return nil, images.ErrNotFound
		},
		createImage: func(_ context.Context, req images.CreateImageRequest) (*images.Image, error) {
			if req.Platform != images.HostPlatformString() {
				t.Fatalf("expected host platform resolve, got %q", req.Platform)
			}
			return nil, images.ErrPlatformNotAvailable
		},
	}

	_, err := resolveImageForCreate(context.Background(), resolver, imageName, "", slog.Default())
	if !errors.Is(err, images.ErrPlatformNotAvailable) {
		t.Fatalf("expected platform-not-available error, got %v", err)
	}
}

// nonHostPlatformString returns the canonical platform of the architecture the
// host is NOT, so tests can exercise the "tag points at the other arch" path
// regardless of which arch they run on.
func nonHostPlatformString() string {
	if images.ImageNeedsHostEmulation("linux/amd64") {
		return "linux/amd64"
	}
	return "linux/arm64"
}

func TestStoredImageNameForCreatePinsExplicitPlatform(t *testing.T) {
	t.Parallel()

	got, err := storedImageNameForCreate(
		"docker.io/library/alpine:3.19",
		&images.Image{Digest: "sha256:amd64"},
	)
	if err != nil {
		t.Fatalf("stored image name: %v", err)
	}
	if got != "docker.io/library/alpine@sha256:amd64" {
		t.Fatalf("expected pinned digest ref, got %q", got)
	}
}

// A no-platform create now resolves a concrete (host) variant, so the instance
// is pinned to that digest too -- keeping it immune to later last-pull-wins tag
// flips rather than re-following the tag on every restart.
func TestStoredImageNameForCreateDefaultPlatformPinsDigest(t *testing.T) {
	t.Parallel()

	got, err := storedImageNameForCreate(
		"docker.io/library/alpine:3.19",
		&images.Image{Digest: "sha256:host"},
	)
	if err != nil {
		t.Fatalf("stored image name: %v", err)
	}
	if got != "docker.io/library/alpine@sha256:host" {
		t.Fatalf("expected pinned digest ref, got %q", got)
	}
}
