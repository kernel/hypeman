// Package imagepush manages outbound image pushes: exporting images from
// hypeman's OCI cache to remote container registries.
//
// A push job resolves a hypeman image to its cached manifest digest, then
// streams the cached blobs to the target reference through lib/registrypush.
// Jobs run on a bounded queue, persist their status to disk, and expose
// in-flight digests so the OCI cache GC keeps the required blobs alive.
package imagepush

import (
	"context"
	"errors"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/kernel/hypeman/lib/images"
)

const (
	StatusQueued  = "queued"
	StatusPushing = "pushing"
	StatusPushed  = "pushed"
	StatusFailed  = "failed"
)

var (
	ErrNotFound      = errors.New("push not found")
	ErrImageNotReady = errors.New("image not ready for push")
	ErrInvalidTarget = errors.New("invalid push target")
)

// PushRequest describes a request to push a hypeman image to a remote registry.
type PushRequest struct {
	// Image is the hypeman image name (tag or digest form).
	Image string
	// Target is the full remote reference, e.g. "registry.example.com/app:v1".
	Target string
	// Insecure allows pushing to plain-HTTP registries.
	Insecure bool
	// Credentials are borrowed for this push only, docker-style: the caller's
	// registry login (e.g. resolved from the client's ~/.docker/config.json)
	// rides along with the request instead of living on the server. They are
	// never persisted or logged; a push interrupted across a restart cannot be
	// recovered and fails instead. When nil, the manager's default provider
	// resolves credentials.
	Credentials *authn.AuthConfig
}

// Push is the state of one push job.
type Push struct {
	ID            string
	Image         string
	Digest        string
	Target        string
	Status        string
	QueuePosition *int
	Error         *string
	Layers        int
	Bytes         int64
	CreatedAt     time.Time
	CompletedAt   *time.Time
}

// ImageResolver resolves a hypeman image name to its stored state.
type ImageResolver interface {
	GetImage(ctx context.Context, name string) (*images.Image, error)
}

// StatusEvent represents a terminal status change for push notifications.
type StatusEvent struct {
	Status string
	Err    error
}

// Manager orchestrates push jobs.
type Manager interface {
	// CreatePush validates the request, persists a queued job, and enqueues it.
	// A request that matches an in-flight job (same digest and target) returns
	// the existing job instead of creating a duplicate; the in-flight job's
	// credentials remain in effect.
	CreatePush(ctx context.Context, req PushRequest) (*Push, error)
	GetPush(ctx context.Context, id string) (*Push, error)
	// ListPushes returns all pushes, newest first.
	ListPushes(ctx context.Context) ([]Push, error)
	// WaitForPush blocks until the push reaches a terminal state (pushed or
	// failed) or the context is cancelled.
	WaitForPush(ctx context.Context, id string) error
	// InProgressDigests returns the manifest digests of queued and pushing
	// jobs so the OCI cache GC can keep their blobs alive mid-push.
	InProgressDigests() []string
}
