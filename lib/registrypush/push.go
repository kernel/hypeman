package registrypush

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kernel/hypeman/lib/ocicache"
	"github.com/kernel/hypeman/lib/paths"
)

// Result describes a completed push.
type Result struct {
	// Reference is the target reference the image was pushed to.
	Reference string
	// Digest is the pushed manifest digest.
	Digest string
	// Layers is the number of layers in the pushed image.
	Layers int
	// Bytes is the total compressed size of the pushed layers.
	Bytes int64
}

// Options configures a push.
type Options struct {
	// Insecure allows pushing to plain-HTTP registries. Localhost registries
	// are always treated as http by go-containerregistry regardless of this
	// flag.
	Insecure bool
}

// Push writes img to the target reference using credentials from provider.
// A nil provider pushes anonymously. Pushed blobs are identical to the
// source image. For OCI images the manifest digest is preserved end to end;
// for Docker v2 inputs the manifest is converted to OCI on push, so the
// pushed digest is the converted digest (see Result.Digest). Registry errors
// are classified with classifyPushError so callers can distinguish
// authentication failures, rate limits, and missing repositories from
// transport failures.
func Push(ctx context.Context, img v1.Image, target string, provider Provider, opts Options) (*Result, error) {
	var refOpts []name.Option
	if opts.Insecure {
		refOpts = append(refOpts, name.Insecure)
	}
	dstRef, err := name.ParseReference(target, refOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse target reference: %w", err)
	}

	auth := authn.Anonymous
	if provider != nil {
		auth, err = provider.Authenticator(ctx, dstRef)
		if err != nil {
			return nil, fmt.Errorf("resolve credentials for %s: %w", dstRef.Context().RegistryStr(), err)
		}
	}

	if err := remote.Write(dstRef, img, remote.WithContext(ctx), remote.WithAuth(auth)); err != nil {
		// Classify the raw registry error. The classifier matches typed
		// transport errors (status/codes), so the destination reference text
		// cannot influence the result.
		return nil, fmt.Errorf("push to %s: %w", dstRef, classifyPushError(err))
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("compute pushed digest: %w", err)
	}

	result := &Result{
		Reference: dstRef.String(),
		Digest:    digest.String(),
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("enumerate layers: %w", err)
	}
	result.Layers = len(layers)
	for _, layer := range layers {
		size, err := layer.Size()
		if err != nil {
			return nil, fmt.Errorf("layer size: %w", err)
		}
		result.Bytes += size
	}
	return result, nil
}

// PushFromCache pushes the image stored in the local OCI cache under the
// given manifest digest. Returns ocicache.ErrNotFound if the digest is not
// in the cache (source phase) or images.ErrNotFound if the destination
// registry reports the repository or manifest as missing (target phase).
func PushFromCache(ctx context.Context, p *paths.Paths, digest, target string, provider Provider, opts Options) (*Result, error) {
	img, err := ocicache.ImageFromCache(p, digest)
	if err != nil {
		return nil, err
	}
	return Push(ctx, img, target, provider, opts)
}
