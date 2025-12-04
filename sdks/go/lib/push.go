// Package lib provides manually-maintained functionality that extends the auto-generated SDK.
package lib

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/onkernel/hypeman-go/internal/requestconfig"
)

// PushConfig holds the configuration needed to push images to hypeman's registry.
// Extract this from a hypeman.Client using ExtractPushConfig.
type PushConfig struct {
	// RegistryHost is the host:port of the hypeman registry (derived from base URL)
	RegistryHost string
	// APIKey is the JWT token for authentication
	APIKey string
}

// ExtractPushConfig extracts the registry host and API key from client options.
// This is needed because the Client struct doesn't expose these values directly.
func ExtractPushConfig(opts []requestconfig.RequestOption) (PushConfig, error) {
	cfg := &requestconfig.RequestConfig{}
	if err := cfg.Apply(opts...); err != nil {
		return PushConfig{}, fmt.Errorf("apply options: %w", err)
	}

	baseURL := cfg.BaseURL
	if baseURL == nil {
		baseURL = cfg.DefaultBaseURL
	}
	if baseURL == nil {
		return PushConfig{}, fmt.Errorf("base URL not configured")
	}

	return PushConfig{
		RegistryHost: baseURL.Host,
		APIKey:       cfg.APIKey,
	}, nil
}

// PushImage pushes a v1.Image to hypeman's OCI registry.
//
// This function works with images from any source supported by go-containerregistry:
//   - Images built with Kaniko, ko, or buildpacks (no Docker needed)
//   - Images pulled from remote registries via remote.Image()
//   - Images loaded from tarballs via tarball.ImageFromPath()
//   - Images from OCI layouts via layout.Image()
//
// The targetName parameter specifies how the image will be named in hypeman
// (e.g., "myapp:latest" or "myorg/myapp:v1.0").
func PushImage(ctx context.Context, cfg PushConfig, img v1.Image, targetName string) error {
	if cfg.RegistryHost == "" {
		return fmt.Errorf("registry host not configured")
	}

	// Build target reference
	targetRef := cfg.RegistryHost + "/" + strings.TrimPrefix(targetName, "/")

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	if err != nil {
		return fmt.Errorf("invalid target reference %q: %w", targetRef, err)
	}

	// Create authenticator with JWT token
	auth := &jwtAuth{token: cfg.APIKey}

	err = remote.Write(dstRef, img,
		remote.WithContext(ctx),
		remote.WithAuth(auth),
	)
	if err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	return nil
}

// Push loads an image from the local Docker daemon and pushes it to hypeman's registry.
//
// This is a convenience function for local development workflows where images
// are built using Docker. For CI/CD pipelines that use Kaniko, ko, or buildpacks,
// use PushImage directly with the v1.Image they produce.
//
// Parameters:
//   - sourceImage: Local Docker image reference (e.g., "myapp:latest")
//   - targetName: Name in hypeman (defaults to sourceImage if empty)
func Push(ctx context.Context, cfg PushConfig, sourceImage, targetName string) error {
	srcRef, err := name.ParseReference(sourceImage)
	if err != nil {
		return fmt.Errorf("invalid source image %q: %w", sourceImage, err)
	}

	img, err := daemon.Image(srcRef)
	if err != nil {
		return fmt.Errorf("load image from docker daemon: %w", err)
	}

	if targetName == "" {
		targetName = sourceImage
	}

	return PushImage(ctx, cfg, img, targetName)
}

// jwtAuth implements authn.Authenticator for JWT bearer token auth
type jwtAuth struct {
	token string
}

func (a *jwtAuth) Authorization() (*authn.AuthConfig, error) {
	if a.token == "" {
		return &authn.AuthConfig{}, nil
	}
	return &authn.AuthConfig{RegistryToken: a.token}, nil
}

// PushFromURL is a convenience function that parses a base URL and API key
// directly, without needing a hypeman.Client. Useful for standalone scripts.
func PushFromURL(ctx context.Context, baseURL, apiKey string, img v1.Image, targetName string) error {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}

	cfg := PushConfig{
		RegistryHost: parsedURL.Host,
		APIKey:       apiKey,
	}

	return PushImage(ctx, cfg, img, targetName)
}
