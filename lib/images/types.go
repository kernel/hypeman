package images

import (
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/kernel/hypeman/lib/tags"
)

// Image represents a container image converted to bootable disk
type Image struct {
	Name          string // Normalized ref (e.g., docker.io/library/alpine:latest)
	Digest        string // Resolved manifest digest (sha256:...)
	Platform      string // Normalized platform (e.g., linux/amd64)
	Status        string
	QueuePosition *int
	Error         *string
	SizeBytes     *int64
	Entrypoint    []string
	Cmd           []string
	Env           map[string]string
	Labels        map[string]string
	Tags          tags.Tags
	WorkingDir    string
	CreatedAt     time.Time
}

// CreateImageRequest represents a request to create an image
type CreateImageRequest struct {
	Name string
	Tags tags.Tags
	// Platform selects which platform variant of a multi-arch image to pull as
	// os/arch[/variant] (e.g., linux/amd64). Empty means the host platform.
	Platform string
	// Credentials are borrowed for this request's registry operations only.
	// They intentionally have no JSON representation and must never be persisted.
	Credentials *authn.AuthConfig `json:"-"`
}
