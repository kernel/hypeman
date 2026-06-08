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

func TestResolveImageForCreateWithoutPlatformUsesExistingImage(t *testing.T) {
	t.Parallel()

	createCalled := false
	resolver := createImageResolverFake{
		getImage: func(context.Context, string) (*images.Image, error) {
			return &images.Image{
				Name:     "docker.io/library/alpine:3.19",
				Platform: "linux/arm64",
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
		t.Fatal("default platform run should use existing image when present")
	}
	if img.Platform != "linux/arm64" {
		t.Fatalf("expected existing image, got %s", img.Platform)
	}
}
