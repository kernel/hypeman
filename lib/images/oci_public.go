package images

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// OCIClient is a public wrapper for system manager to use OCI operations
type OCIClient struct {
	client *ociClient
}

// NewOCIClient creates a new OCI client (public for system manager).
// If keychain is nil, the Docker config keychain is used for registry auth.
func NewOCIClient(cacheDir string, keychain authn.Keychain) (*OCIClient, error) {
	client, err := newOCIClient(cacheDir, keychain)
	if err != nil {
		return nil, err
	}
	return &OCIClient{client: client}, nil
}

// InspectManifest inspects a remote image to get its digest (public for system manager).
// Always targets Linux platform since hypeman VMs are Linux guests.
func (c *OCIClient) InspectManifest(ctx context.Context, imageRef string) (string, error) {
	return c.client.inspectManifest(ctx, imageRef)
}

// InspectManifestForPlatform inspects a remote image to get the digest for a
// specific platform variant. Empty platform uses the host platform.
func (c *OCIClient) InspectManifestForPlatform(ctx context.Context, imageRef, platform string) (string, error) {
	if strings.TrimSpace(platform) == "" {
		return c.InspectManifest(ctx, imageRef)
	}
	resolvedPlatform, err := ParsePlatform(platform)
	if err != nil {
		return "", err
	}
	return c.client.inspectManifestWithPlatform(ctx, imageRef, resolvedPlatform.ToGCR())
}

// InspectManifestForLinux is an alias for InspectManifest (all images target Linux)
func (c *OCIClient) InspectManifestForLinux(ctx context.Context, imageRef string) (string, error) {
	return c.InspectManifest(ctx, imageRef)
}

// PullAndUnpack pulls an OCI image and unpacks it to a directory (public for system manager).
// Always targets Linux platform since hypeman VMs are Linux guests.
func (c *OCIClient) PullAndUnpack(ctx context.Context, imageRef, digest, exportDir string) error {
	_, err := c.client.pullAndExport(ctx, imageRef, digest, exportDir)
	if err != nil {
		return fmt.Errorf("pull and unpack: %w", err)
	}
	return nil
}

// PullAndUnpackForLinux is an alias for PullAndUnpack (all images target Linux)
func (c *OCIClient) PullAndUnpackForLinux(ctx context.Context, imageRef, digest, exportDir string) error {
	return c.PullAndUnpack(ctx, imageRef, digest, exportDir)
}
