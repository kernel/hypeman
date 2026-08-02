package builds

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// remotePublisher copies completed build images from the local registry to a
// configured remote registry. All credentials stay on the host: the local
// pull uses a short-lived token generated in-process, and remote credentials
// are read from a host-side Docker client config. Neither is sent to builder
// VMs.
type remotePublisher struct {
	config        PublishConfig
	localRegistry string
	tokenGen      *RegistryTokenGenerator
	keychain      authn.Keychain
}

func newRemotePublisher(cfg PublishConfig, localRegistry string, tokenGen *RegistryTokenGenerator) (BuildPublisher, error) {
	keychain, err := dockerFileKeychain(cfg.CredentialsFile)
	if err != nil {
		return nil, err
	}
	return &remotePublisher{
		config:        cfg,
		localRegistry: localRegistry,
		tokenGen:      tokenGen,
		keychain:      keychain,
	}, nil
}

// Publish copies the completed local image to
// <remote-registry>/<prefix>/<build-id>@<digest> and verifies the remote
// manifest matches the build digest before returning the immutable remote
// reference.
func (p *remotePublisher) Publish(ctx context.Context, localRef, digest string) (string, error) {
	if digest == "" {
		return "", fmt.Errorf("publish: digest is required")
	}

	srcRef, err := name.ParseReference(fmt.Sprintf("%s/%s@%s", p.localRegistry, localRef, digest))
	if err != nil {
		return "", fmt.Errorf("publish: parse local reference: %w", err)
	}
	dstRef, err := name.NewDigest(fmt.Sprintf("%s@%s", publishedRepository(p.config, localRef), digest))
	if err != nil {
		return "", fmt.Errorf("publish: parse remote reference: %w", err)
	}

	// Short-lived pull token for the local registry, generated on the host.
	token, err := p.tokenGen.GenerateToken(localRef, []RepoPermission{{Repo: localRef, Scope: "pull"}}, 10*time.Minute)
	if err != nil {
		return "", fmt.Errorf("publish: generate local registry token: %w", err)
	}
	localAuth := authn.FromConfig(authn.AuthConfig{Username: token, Password: "x"})

	img, err := remote.Image(srcRef, remote.WithContext(ctx), remote.WithAuth(localAuth))
	if err != nil {
		return "", fmt.Errorf("publish: fetch local image: %w", err)
	}

	// Blobs are content-addressed and uploads are idempotent (existing blobs
	// are skipped), so retrying a failed publish resumes safely.
	if err := pushWithRetry(ctx, dstRef, img, p.keychain); err != nil {
		return "", fmt.Errorf("publish: push to remote registry: %w", err)
	}

	if err := verifyRemoteDigest(ctx, dstRef, digest, p.keychain); err != nil {
		return "", err
	}

	return dstRef.String(), nil
}

// pushWithRetry pushes the image, retrying transient registry errors.
func pushWithRetry(ctx context.Context, ref name.Digest, img v1.Image, keychain authn.Keychain) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = remote.Write(ref, img, remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain))
		if err == nil {
			return nil
		}
		var terr *transport.Error
		if !errors.As(err, &terr) {
			// Non-registry errors (DNS, connection reset, EOF) are treated as
			// transient; the upload is idempotent so retrying is safe.
		} else if !terr.Temporary() {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return err
}

// verifyRemoteDigest reads the remote manifest and fails unless its digest
// matches the build digest.
func verifyRemoteDigest(ctx context.Context, ref name.Digest, want string, keychain authn.Keychain) error {
	head, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain))
	if err != nil {
		return fmt.Errorf("publish: verify remote manifest: %w", err)
	}
	if head.Digest.String() != want {
		return fmt.Errorf("publish: verify remote manifest: digest mismatch: remote has %s, want %s", head.Digest, want)
	}
	return nil
}

// publishedRepository derives the remote repository for a completed local
// build reference: the last path segment of the local repository (the build
// ID for the canonical "builds/<id>" reference) under the configured prefix.
func publishedRepository(cfg PublishConfig, localRef string) string {
	repo := localRef
	if idx := strings.LastIndex(repo, "@"); idx != -1 {
		repo = repo[:idx]
	}
	if idx := strings.LastIndex(repo, ":"); idx > strings.LastIndex(repo, "/") {
		repo = repo[:idx]
	}
	name := repo
	if idx := strings.LastIndex(repo, "/"); idx != -1 {
		name = repo[idx+1:]
	}
	return strings.TrimSuffix(cfg.Registry, "/") + "/" + strings.Trim(cfg.RepositoryPrefix, "/") + "/" + name
}

// dockerConfigFile is the subset of a Docker client config used for
// registry authentication.
type dockerConfigFile struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
}

// dockerConfigKeychain resolves registry credentials from a parsed Docker
// client config, falling back to anonymous access for unknown registries.
type dockerConfigKeychain struct {
	auths map[string]authn.AuthConfig
}

func (k dockerConfigKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if cfg, ok := k.auths[r.RegistryStr()]; ok {
		return authn.FromConfig(cfg), nil
	}
	return authn.Anonymous, nil
}

type anonymousKeychain struct{}

func (anonymousKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return authn.Anonymous, nil
}

// dockerFileKeychain loads registry credentials from a host-side Docker
// client config. An empty path yields an anonymous keychain.
func dockerFileKeychain(path string) (authn.Keychain, error) {
	if path == "" {
		return anonymousKeychain{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("publish: read credentials file: %w", err)
	}

	var cfg dockerConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("publish: parse credentials file: %w", err)
	}

	auths := make(map[string]authn.AuthConfig, len(cfg.Auths))
	for host, entry := range cfg.Auths {
		ac := authn.AuthConfig{Username: entry.Username, Password: entry.Password}
		if entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return nil, fmt.Errorf("publish: decode credentials for %s: %w", host, err)
			}
			user, pass, ok := strings.Cut(string(decoded), ":")
			if !ok {
				return nil, fmt.Errorf("publish: malformed credentials for %s", host)
			}
			ac.Username, ac.Password = user, pass
		}
		host = strings.TrimPrefix(host, "https://")
		host = strings.TrimPrefix(host, "http://")
		host = strings.TrimSuffix(host, "/")
		auths[host] = ac
	}

	return dockerConfigKeychain{auths: auths}, nil
}
