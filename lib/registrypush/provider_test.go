package registrypush

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/kernel/hypeman/lib/paths"
)

// writeToCache stores an image's blobs in the OCI cache under p and returns
// the manifest digest.
func writeToCache(p *paths.Paths, img v1.Image) (string, error) {
	blobDir := p.OCICacheBlobDir()
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return "", fmt.Errorf("layers: %w", err)
	}
	for _, layer := range layers {
		rc, err := layer.Compressed()
		if err != nil {
			return "", fmt.Errorf("layer reader: %w", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("read layer: %w", err)
		}
		hash, err := layer.Digest()
		if err != nil {
			return "", fmt.Errorf("layer digest: %w", err)
		}
		if err := os.WriteFile(filepath.Join(blobDir, hash.Hex), data, 0644); err != nil {
			return "", fmt.Errorf("write layer blob: %w", err)
		}
	}

	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return "", fmt.Errorf("raw config: %w", err)
	}
	configHash, err := img.ConfigName()
	if err != nil {
		return "", fmt.Errorf("config name: %w", err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, configHash.Hex), rawConfig, 0644); err != nil {
		return "", fmt.Errorf("write config blob: %w", err)
	}

	rawManifest, err := img.RawManifest()
	if err != nil {
		return "", fmt.Errorf("raw manifest: %w", err)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, digest.Hex), rawManifest, 0644); err != nil {
		return "", fmt.Errorf("write manifest blob: %w", err)
	}

	return digest.String(), nil
}

// fakeProvider records calls and returns a fixed authenticator.
type fakeProvider struct {
	auth  authn.Authenticator
	calls int
}

func (f *fakeProvider) Authenticator(_ context.Context, _ name.Reference) (authn.Authenticator, error) {
	f.calls++
	return f.auth, nil
}

func TestMultiRoutesByHost(t *testing.T) {
	hostAuth := &authn.Basic{Username: "host-user", Password: "host-pass"}
	defaultAuth := &authn.Basic{Username: "default-user", Password: "default-pass"}
	hostProvider := &fakeProvider{auth: hostAuth}
	defaultProvider := &fakeProvider{auth: defaultAuth}

	multi := &Multi{
		Default: defaultProvider,
		ByHost: map[string]Provider{
			"registry.example.com": hostProvider,
		},
	}

	ref, err := name.ParseReference("registry.example.com/team/app:v1")
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	auth, err := multi.Authenticator(context.Background(), ref)
	if err != nil {
		t.Fatalf("Authenticator: %v", err)
	}
	if auth != hostAuth {
		t.Error("expected the host-specific provider to be used")
	}
	if hostProvider.calls != 1 || defaultProvider.calls != 0 {
		t.Errorf("calls = host:%d default:%d, want host:1 default:0", hostProvider.calls, defaultProvider.calls)
	}

	otherRef, err := name.ParseReference("other.example.com/team/app:v1")
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	auth, err = multi.Authenticator(context.Background(), otherRef)
	if err != nil {
		t.Fatalf("Authenticator: %v", err)
	}
	if auth != defaultAuth {
		t.Error("expected the default provider to be used for unmatched hosts")
	}
}

func TestMultiNilDefaultIsAnonymous(t *testing.T) {
	multi := &Multi{}

	ref, err := name.ParseReference("registry.example.com/team/app:v1")
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	auth, err := multi.Authenticator(context.Background(), ref)
	if err != nil {
		t.Fatalf("Authenticator: %v", err)
	}
	if auth != authn.Anonymous {
		t.Error("expected anonymous auth with no matching host and nil default")
	}
}

func TestKeychainProviderReadsDockerConfig(t *testing.T) {
	configDir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("testuser:testpass"))
	config := fmt.Sprintf(`{"auths":{"127.0.0.1:5999":{"auth":"%s"}}}`, auth)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(config), 0600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", configDir)

	ref, err := name.ParseReference("127.0.0.1:5999/team/app:v1")
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}

	provider := &KeychainProvider{}
	authenticator, err := provider.Authenticator(context.Background(), ref)
	if err != nil {
		t.Fatalf("Authenticator: %v", err)
	}
	cfg, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	if cfg.Username != "testuser" || cfg.Password != "testpass" {
		t.Errorf("credentials = %s/%s, want testuser/testpass", cfg.Username, cfg.Password)
	}
}
