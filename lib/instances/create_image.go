package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/images"
)

type createImageResolver interface {
	CreateImage(ctx context.Context, req images.CreateImageRequest) (*images.Image, error)
	GetImage(ctx context.Context, name string) (*images.Image, error)
	WaitForReady(ctx context.Context, name string) error
}

func resolveImageForCreate(ctx context.Context, imageManager createImageResolver, imageName, platform string, log *slog.Logger) (*images.Image, error) {
	if strings.TrimSpace(platform) != "" {
		log.InfoContext(ctx, "resolving image for requested platform", "image", imageName, "platform", platform)
		img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageName, Platform: platform})
		if err != nil {
			return nil, fmt.Errorf("resolve image %s for platform %s: %w", imageName, platform, err)
		}
		pinned, err := pinnedImageName(imageName, img.Digest)
		if err != nil {
			return nil, err
		}
		img, err = awaitReady(ctx, imageManager, img, pinned, log)
		if err != nil {
			return nil, err
		}
		if err := validateResolvedImagePlatform(img, platform); err != nil {
			return nil, err
		}
		return img, nil
	}

	img, err := imageManager.GetImage(ctx, imageName)
	if err == nil {
		return img, nil
	}
	if !errors.Is(err, images.ErrNotFound) {
		return nil, fmt.Errorf("get image: %w", err)
	}

	log.InfoContext(ctx, "image not found locally, auto-pulling", "image", imageName)
	img, err = imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageName})
	if err != nil {
		return nil, fmt.Errorf("auto-pull image %s: %w", imageName, err)
	}
	img, err = awaitReady(ctx, imageManager, img, img.Name, log)
	if err != nil {
		return nil, err
	}
	return img, nil
}

func awaitReady(ctx context.Context, imageManager createImageResolver, img *images.Image, imageName string, log *slog.Logger) (*images.Image, error) {
	if img.Status == images.StatusReady {
		return img, nil
	}
	if err := waitForImagePull(ctx, imageManager, imageName, log); err != nil {
		return nil, err
	}
	img, err := imageManager.GetImage(ctx, imageName)
	if err != nil {
		return nil, fmt.Errorf("get image after pull: %w", err)
	}
	return img, nil
}

func waitForImagePull(ctx context.Context, imageManager createImageResolver, imageName string, log *slog.Logger) error {
	pullCtx, pullCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pullCancel()
	if err := imageManager.WaitForReady(pullCtx, imageName); err != nil {
		log.InfoContext(ctx, "image pull not ready within timeout, pull continues in background", "image", imageName, "error", err)
		return fmt.Errorf("%w: image %s is being pulled, please try again shortly", ErrImageNotReady, imageName)
	}
	return nil
}

func pinnedImageName(imageName, digest string) (string, error) {
	if strings.TrimSpace(digest) == "" {
		return "", fmt.Errorf("%w: resolved image %s has no digest", ErrImageNotReady, imageName)
	}
	parsed, err := images.ParseNormalizedRef(imageName)
	if err != nil {
		return "", fmt.Errorf("parse image ref %q: %w", imageName, err)
	}
	return parsed.Repository() + "@" + digest, nil
}

func storedImageNameForCreate(imageName, platform string, img *images.Image) (string, error) {
	if strings.TrimSpace(platform) == "" {
		return imageName, nil
	}
	if img == nil {
		return "", fmt.Errorf("%w: image did not resolve", ErrImageNotReady)
	}
	return pinnedImageName(imageName, img.Digest)
}

func validateResolvedImagePlatform(img *images.Image, requestedPlatform string) error {
	if img == nil {
		return fmt.Errorf("%w: image did not resolve", ErrImageNotReady)
	}
	want, err := images.ParsePlatform(requestedPlatform)
	if err != nil {
		return err
	}
	got, err := images.ParsePlatform(img.Platform)
	if err != nil {
		return fmt.Errorf("%w: resolved image %s has invalid platform %q", images.ErrInvalidPlatform, img.Name, img.Platform)
	}
	if !want.Matches(got) {
		return fmt.Errorf("%w: requested %s but resolved image %s is %s", images.ErrInvalidPlatform, want, img.Name, got)
	}
	return nil
}
