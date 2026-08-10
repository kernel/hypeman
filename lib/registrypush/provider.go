// Package registrypush pushes images from hypeman's OCI cache to remote
// container registries.
//
// Credentials are resolved by Provider implementations keyed on the target
// reference. The push itself is registry-agnostic; registries with custom
// auth flows plug in as additional providers without touching the push path.
package registrypush

import (
	"context"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// Provider resolves credentials for pushing to a target reference.
type Provider interface {
	Authenticator(ctx context.Context, ref name.Reference) (authn.Authenticator, error)
}

// KeychainProvider resolves credentials from a Docker-style keychain. A nil
// Keychain uses authn.DefaultKeychain, which reads ~/.docker/config.json
// including any configured credential helpers.
type KeychainProvider struct {
	Keychain authn.Keychain
}

// Authenticator resolves credentials for the target registry from the
// keychain. The caller's context is forwarded so context-aware keychains
// (authn.ContextKeychain) receive cancellation and deadlines.
func (p *KeychainProvider) Authenticator(ctx context.Context, ref name.Reference) (authn.Authenticator, error) {
	kc := p.Keychain
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	return authn.Resolve(ctx, kc, ref.Context())
}

// StaticProvider always returns the same fixed credentials, regardless of
// target. Useful for a single pre-configured destination.
type StaticProvider struct {
	Config authn.AuthConfig
}

// Authenticator returns the configured credentials.
func (p *StaticProvider) Authenticator(_ context.Context, _ name.Reference) (authn.Authenticator, error) {
	return authn.FromConfig(p.Config), nil
}

// Multi routes credential resolution by registry host. The provider whose key
// matches the target's registry host wins; everything else falls back to
// Default. Keys must match the canonical host form reported by
// name.Registry.RegistryStr (e.g. "index.docker.io" for Docker Hub). A nil
// Default resolves anonymously.
type Multi struct {
	Default Provider
	ByHost  map[string]Provider
}

// Authenticator dispatches to the provider registered for the target host.
func (m *Multi) Authenticator(ctx context.Context, ref name.Reference) (authn.Authenticator, error) {
	if prov, ok := m.ByHost[ref.Context().RegistryStr()]; ok {
		return prov.Authenticator(ctx, ref)
	}
	if m.Default == nil {
		return authn.Anonymous, nil
	}
	return m.Default.Authenticator(ctx, ref)
}
