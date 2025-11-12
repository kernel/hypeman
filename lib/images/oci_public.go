package images

import (
	"context"
	"fmt"
)

// OCIClient is a public wrapper for system manager to use OCI operations
type OCIClient struct {
	client *ociClient
}

// NewOCIClient creates a new OCI client (public for system manager)
func NewOCIClient(cacheDir string) (*OCIClient, error) {
	client, err := newOCIClient(cacheDir)
	if err != nil {
		return nil, err
	}
	return &OCIClient{client: client}, nil
}

// InspectManifest inspects a remote image to get its digest (public for system manager)
func (c *OCIClient) InspectManifest(ctx context.Context, imageRef string) (string, error) {
	return c.client.inspectManifest(ctx, imageRef)
}

// PullAndUnpack pulls an OCI image and unpacks it to a directory (public for system manager)
func (c *OCIClient) PullAndUnpack(ctx context.Context, imageRef, digest, exportDir string) error {
	_, err := c.client.pullAndExport(ctx, imageRef, digest, exportDir)
	if err != nil {
		return fmt.Errorf("pull and unpack: %w", err)
	}
	return nil
}

