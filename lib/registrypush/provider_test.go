package registrypush

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

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

// ctxKeychain implements authn.ContextKeychain and records the context it
// receives, so tests can assert the caller's context is forwarded rather
// than dropped.
type ctxKeychain struct {
	gotCtx context.Context
}

func (c *ctxKeychain) Resolve(_ authn.Resource) (authn.Authenticator, error) {
	return authn.Anonymous, nil
}

func (c *ctxKeychain) ResolveContext(ctx context.Context, _ authn.Resource) (authn.Authenticator, error) {
	c.gotCtx = ctx
	return authn.Anonymous, nil
}

func TestKeychainProviderForwardsContext(t *testing.T) {
	t.Parallel()

	kc := &ctxKeychain{}
	provider := &KeychainProvider{Keychain: kc}

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")

	ref, err := name.ParseReference("registry.example.com/team/app:v1")
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}

	if _, err := provider.Authenticator(ctx, ref); err != nil {
		t.Fatalf("Authenticator: %v", err)
	}
	if kc.gotCtx == nil {
		t.Fatal("keychain did not receive a context")
	}
	if got := kc.gotCtx.Value(ctxKey{}); got != "sentinel" {
		t.Errorf("context value = %v, want sentinel (context was not forwarded)", got)
	}
}
