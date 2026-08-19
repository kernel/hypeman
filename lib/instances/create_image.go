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
	// An empty platform means the host platform: a no-platform run must boot a
	// host-native guest and never silently emulate, so we resolve the host variant
	// explicitly rather than follow the (last-pull-wins) tag, which may point at a
	// non-host arch. Tag last-pull-wins still governs image addressing.
	resolvePlatform := platform
	if strings.TrimSpace(resolvePlatform) == "" {
		// Fast-path a cached image only if its recorded platform is non-empty AND
		// host-native; an empty/unknown platform (e.g. a legacy record) is not
		// assumed to be the host and falls through to host-pinned resolution.
		if img, err := imageManager.GetImage(ctx, imageName); err == nil {
			if strings.TrimSpace(img.Platform) == images.HostPlatformString() {
				return img, nil
			}
		} else if !errors.Is(err, images.ErrNotFound) {
			return nil, fmt.Errorf("get image: %w", err)
		}
		// Materialize host so validateResolvedImagePlatform has a concrete target.
		resolvePlatform = images.HostPlatformString()
		log.InfoContext(ctx, "no platform requested, resolving host platform", "image", imageName, "platform", resolvePlatform)
	} else {
		log.InfoContext(ctx, "resolving image for requested platform", "image", imageName, "platform", resolvePlatform)
	}

	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageName, Platform: resolvePlatform})
	if err != nil {
		return nil, fmt.Errorf("resolve image %s for platform %s: %w", imageName, resolvePlatform, err)
	}
	pinned, err := pinnedImageName(imageName, img.Digest)
	if err != nil {
		return nil, err
	}
	img, err = awaitReady(ctx, imageManager, img, pinned, log)
	if err != nil {
		return nil, err
	}
	if err := validateResolvedImagePlatform(img, resolvePlatform); err != nil {
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

// storedImageNameForCreate pins the instance to the exact digest resolved at
// create time. Pinning the digest -- not the tag -- keeps the instance booting
// the same arch across restarts, immune to a later last-pull-wins tag flip; this
// matters even for a no-platform create, which now resolves the host variant.
func storedImageNameForCreate(imageName string, img *images.Image) (string, error) {
	if img == nil {
		return "", fmt.Errorf("%w: image did not resolve", ErrImageNotReady)
	}
	return pinnedImageName(imageName, img.Digest)
}

// bootImageRef returns the reference used to locate an instance's rootfs at
// boot/start/restore. It prefers the digest-pinned ResolvedImage (so a mutable
// tag can't drift the instance to a different image/arch) and falls back to the
// caller-facing Image for instances created before ResolvedImage was persisted.
func bootImageRef(stored *StoredMetadata) string {
	if stored == nil {
		return ""
	}
	if strings.TrimSpace(stored.ResolvedImage) != "" {
		return stored.ResolvedImage
	}
	return stored.Image
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
