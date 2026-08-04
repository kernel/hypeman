// Package registryauth builds an OCI registry credential keychain from
// hypeman configuration so image pulls can authenticate against any remote
// registry, not just the ones present in ~/.docker/config.json.
package registryauth

import (
	"fmt"
	"os"
	"path"

	"github.com/google/go-containerregistry/pkg/authn"
)

// Supported credential kinds.
const (
	// KindStatic authenticates with a fixed username and password.
	KindStatic = "static"
	// KindECR fetches short-lived tokens from AWS ECR's
	// GetAuthorizationToken API, refreshing them before expiry.
	KindECR = "ecr"
	// KindDockerConfig defers to the Docker config keychain
	// (~/.docker/config.json) for the matched registries.
	KindDockerConfig = "docker-config"
)

// RegistryConfig configures credentials for pulling images from remote
// registries. Entries are matched in order against the image reference's
// registry host; the first match wins. Registries matching no entry fall
// back to the Docker config keychain.
type RegistryConfig struct {
	// Host is a glob pattern matched against the registry host, e.g.
	// "docker.io", "ghcr.io", or "*.dkr.ecr.us-east-1.amazonaws.com".
	Host string `koanf:"host"`

	// Kind selects the credential provider: "static", "ecr", or
	// "docker-config".
	Kind string `koanf:"kind"`

	// Username and Password authenticate "static" registries. Password
	// supports ${VAR} environment expansion.
	Username string `koanf:"username"`
	Password string `koanf:"password"`

	// AccessKeyID and SecretAccessKey optionally pin the AWS credentials
	// used by "ecr" registries. When empty, the standard AWS credential
	// chain (environment, shared config) is used.
	AccessKeyID     string `koanf:"access_key_id"`
	SecretAccessKey string `koanf:"secret_access_key"`
}

// Validate checks a single registry entry.
func (r RegistryConfig) Validate() error {
	if r.Host == "" {
		return fmt.Errorf("registries: host is required")
	}
	switch r.Kind {
	case KindStatic:
		if r.Username == "" || r.Password == "" {
			return fmt.Errorf("registries[%s]: static kind requires username and password", r.Host)
		}
	case KindECR:
		if (r.AccessKeyID == "") != (r.SecretAccessKey == "") {
			return fmt.Errorf("registries[%s]: ecr kind requires both access_key_id and secret_access_key", r.Host)
		}
	case KindDockerConfig:
	default:
		return fmt.Errorf("registries[%s]: unknown kind %q (expected %q, %q, or %q)", r.Host, r.Kind, KindStatic, KindECR, KindDockerConfig)
	}
	return nil
}

// NewKeychain builds a keychain from the configured registry entries,
// falling back to the Docker config keychain for unmatched registries. An
// empty config returns the Docker config keychain alone.
func NewKeychain(cfgs []RegistryConfig) (authn.Keychain, error) {
	for _, cfg := range cfgs {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}
	if len(cfgs) == 0 {
		return authn.DefaultKeychain, nil
	}

	keychains := make([]authn.Keychain, 0, len(cfgs)+1)
	for _, cfg := range cfgs {
		switch cfg.Kind {
		case KindStatic:
			keychains = append(keychains, &staticKeychain{
				pattern: cfg.Host,
				auth: authn.FromConfig(authn.AuthConfig{
					Username: cfg.Username,
					Password: os.Expand(cfg.Password, os.Getenv),
				}),
			})
		case KindECR:
			keychains = append(keychains, newECRKeychain(cfg))
		case KindDockerConfig:
			keychains = append(keychains, authn.DefaultKeychain)
		}
	}
	keychains = append(keychains, authn.DefaultKeychain)
	return authn.NewMultiKeychain(keychains...), nil
}

// staticKeychain serves fixed credentials for registries matching a host
// pattern.
type staticKeychain struct {
	pattern string
	auth    authn.Authenticator
}

func (k *staticKeychain) Resolve(reg authn.Resource) (authn.Authenticator, error) {
	if !matchHost(k.pattern, reg.RegistryStr()) {
		return authn.Anonymous, nil
	}
	return k.auth, nil
}

// matchHost matches a glob pattern against a registry host. Registry hosts
// never contain slashes, so path.Match's single-segment wildcards suffice.
// go-containerregistry canonicalizes docker.io to index.docker.io, so
// patterns written either way match Docker Hub references.
func matchHost(pattern, host string) bool {
	if matched, _ := path.Match(pattern, host); matched {
		return true
	}
	if host == "index.docker.io" {
		matched, _ := path.Match(pattern, "docker.io")
		return matched
	}
	if host == "docker.io" {
		matched, _ := path.Match(pattern, "index.docker.io")
		return matched
	}
	return false
}
