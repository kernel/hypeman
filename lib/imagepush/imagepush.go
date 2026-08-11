// Package imagepush manages outbound image pushes: exporting images from
// hypeman's OCI cache to remote container registries.
//
// A push job resolves a hypeman image to its cached manifest digest, then
// streams the cached blobs to the target reference through lib/registrypush.
// Jobs run on a bounded queue, persist their status to disk, and expose
// in-flight digests so the OCI cache GC keeps the required blobs alive.
package imagepush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/kernel/hypeman/lib/images"
)

const (
	StatusQueued  = "queued"
	StatusPushing = "pushing"
	StatusPushed  = "pushed"
	StatusFailed  = "failed"

	// DefaultPushTimeout bounds a single registry export so a wedged registry
	// cannot pin a queue slot forever.
	DefaultPushTimeout = 30 * time.Minute
)

var (
	ErrNotFound      = errors.New("push not found")
	ErrImageNotReady = errors.New("image not ready for push")
	ErrInvalidTarget = errors.New("invalid push target")
	// ErrCredentialConflict is returned when a push request matches an
	// in-flight job but its credential intent differs from that job's. The
	// manager never stores credential values, so it can only detect a
	// presence mismatch; two requests that both borrow different credentials
	// for the same target are indistinguishable and merge.
	ErrCredentialConflict = errors.New("push already in flight with different credentials")
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

// credsPresent reports whether a credential config carries any credential
// material; an empty non-nil config is treated as anonymous.
func credsPresent(c *authn.AuthConfig) bool {
	if c == nil {
		return false
	}
	return c.Username != "" || c.Password != "" || c.Auth != "" || c.IdentityToken != "" || c.RegistryToken != ""
}

// credFingerprint hashes borrowed credentials so in-flight dedup can tell
// "same login" from "different login" without retaining the secret material.
// Anonymous configs (nil or empty) all share the empty fingerprint.
func credFingerprint(c *authn.AuthConfig) string {
	if !credsPresent(c) {
		return ""
	}
	// Normalize the precomputed base64 "user:pass" Auth shorthand into its
	// username/password parts so the same login supplied either way hashes
	// identically (AuthConfig.UnmarshalJSON already expands it, but a config
	// built in code may not have gone through JSON).
	username, password := c.Username, c.Password
	if c.Auth != "" {
		if decoded, err := base64.StdEncoding.DecodeString(c.Auth); err == nil {
			if i := bytes.IndexByte(decoded, ':'); i >= 0 {
				username, password = string(decoded[:i]), string(decoded[i+1:])
			}
		}
	}
	// IdentityToken and RegistryToken are distinct auth modes (token/registry-
	// scoped) and are kept as-is; they can carry a different identity than the
	// basic-auth pair.
	return credentialFingerprint(username, password, c.IdentityToken, c.RegistryToken)
}

func credentialFingerprint(username, password, identityToken, registryToken string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", username, password, identityToken, registryToken)))
	return hex.EncodeToString(sum[:])
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
	// credentials remain in effect. A request whose credential presence
	// differs from the in-flight job's (one borrowed, one not) returns
	// ErrCredentialConflict instead of silently merging under the wrong auth.
	CreatePush(ctx context.Context, req PushRequest) (*Push, error)
	GetPush(ctx context.Context, id string) (*Push, error)
	// ListPushes returns all pushes, newest first.
	ListPushes(ctx context.Context) ([]Push, error)
	// WaitForPush blocks until the push reaches a terminal state (pushed or
	// failed) or the context is cancelled. The HTTP API currently polls the
	// persisted job instead; this remains available to in-process callers.
	WaitForPush(ctx context.Context, id string) error
	// LiveCacheManifestDigests implements ocicachegc.RootsProvider: the
	// manifest digests of queued and pushing jobs, so the OCI cache GC keeps
	// their blobs alive mid-push.
	LiveCacheManifestDigests() []string
}
