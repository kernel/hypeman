//go:build linux && amd64

package instances

import (
	"context"

	"github.com/kernel/hypeman/lib/images"
)

type windowsFixtureImageManager struct {
	images.Manager
	image *images.Image
}

func (m windowsFixtureImageManager) CreateImage(context.Context, images.CreateImageRequest) (*images.Image, error) {
	copy := *m.image
	return &copy, nil
}

func (m windowsFixtureImageManager) GetImage(context.Context, string) (*images.Image, error) {
	copy := *m.image
	return &copy, nil
}

func (m windowsFixtureImageManager) WaitForReady(context.Context, string) error { return nil }
