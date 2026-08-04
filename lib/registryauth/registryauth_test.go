package registryauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

func registry(t *testing.T, host string) name.Registry {
	t.Helper()
	reg, err := name.NewRegistry(host)
	if err != nil {
		t.Fatalf("parse registry %q: %v", host, err)
	}
	return reg
}

func TestMatchHost(t *testing.T) {
	tests := []struct {
		pattern string
		host    string
		want    bool
	}{
		{"docker.io", "docker.io", true},
		{"docker.io", "index.docker.io", true},
		{"index.docker.io", "docker.io", true},
		{"ghcr.io", "ghcr.io", true},
		{"ghcr.io", "docker.io", false},
		{"*.dkr.ecr.us-east-1.amazonaws.com", "123456789012.dkr.ecr.us-east-1.amazonaws.com", true},
		{"*.dkr.ecr.us-east-1.amazonaws.com", "123456789012.dkr.ecr.eu-west-1.amazonaws.com", false},
		{"*.dkr.ecr.*.amazonaws.com", "123456789012.dkr.ecr.eu-west-1.amazonaws.com", true},
		{"localhost:5000", "localhost:5000", true},
		{"*", "anything.io", true},
	}
	for _, tt := range tests {
		if got := matchHost(tt.pattern, tt.host); got != tt.want {
			t.Errorf("matchHost(%q, %q) = %v, want %v", tt.pattern, tt.host, got, tt.want)
		}
	}
}

func TestECRRegionFromHost(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com", "us-east-1"},
		{"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com", "us-gov-west-1"},
		{"123456789012.dkr.ecr.eu-west-1.amazonaws.com:443", "eu-west-1"},
		{"public.ecr.aws", ""},
		{"docker.io", ""},
		{"not-ecr.amazonaws.com", ""},
	}
	for _, tt := range tests {
		if got := ecrRegionFromHost(tt.host); got != tt.want {
			t.Errorf("ecrRegionFromHost(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

func TestRegistryConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     RegistryConfig
		wantErr bool
	}{
		{"valid static", RegistryConfig{Host: "docker.io", Kind: KindStatic, Username: "u", Password: "p"}, false},
		{"static missing password", RegistryConfig{Host: "docker.io", Kind: KindStatic, Username: "u"}, true},
		{"valid ecr", RegistryConfig{Host: "*.dkr.ecr.us-east-1.amazonaws.com", Kind: KindECR}, false},
		{"valid ecr with keys", RegistryConfig{Host: "*.dkr.ecr.us-east-1.amazonaws.com", Kind: KindECR, AccessKeyID: "AK", SecretAccessKey: "SK"}, false},
		{"ecr half credentials", RegistryConfig{Host: "*.dkr.ecr.us-east-1.amazonaws.com", Kind: KindECR, AccessKeyID: "AK"}, true},
		{"valid docker-config", RegistryConfig{Host: "*", Kind: KindDockerConfig}, false},
		{"missing host", RegistryConfig{Kind: KindStatic, Username: "u", Password: "p"}, true},
		{"unknown kind", RegistryConfig{Host: "docker.io", Kind: "bogus"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewKeychainEmptyConfig(t *testing.T) {
	kc, err := NewKeychain(nil)
	if err != nil {
		t.Fatalf("NewKeychain(nil): %v", err)
	}
	if kc != authn.DefaultKeychain {
		t.Fatal("empty config should return the Docker config keychain")
	}
}

func TestNewKeychainValidationError(t *testing.T) {
	_, err := NewKeychain([]RegistryConfig{{Host: "docker.io", Kind: "bogus"}})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestStaticKeychain(t *testing.T) {
	t.Setenv("TEST_REGISTRY_PASSWORD", "expanded-secret")

	kc, err := NewKeychain([]RegistryConfig{{
		Host:     "docker.io",
		Kind:     KindStatic,
		Username: "onkernel",
		Password: "${TEST_REGISTRY_PASSWORD}",
	}})
	if err != nil {
		t.Fatalf("NewKeychain: %v", err)
	}

	auth, err := kc.Resolve(registry(t, "index.docker.io"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cfg, err := auth.Authorization()
	if err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	if cfg.Username != "onkernel" || cfg.Password != "expanded-secret" {
		t.Fatalf("got credentials %q/%q, want onkernel/expanded-secret", cfg.Username, cfg.Password)
	}
}

func TestStaticKeychainFallsThrough(t *testing.T) {
	// Point DOCKER_CONFIG at an empty dir so the DefaultKeychain fallback
	// resolves deterministically to anonymous.
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	kc, err := NewKeychain([]RegistryConfig{{
		Host:     "docker.io",
		Kind:     KindStatic,
		Username: "u",
		Password: "p",
	}})
	if err != nil {
		t.Fatalf("NewKeychain: %v", err)
	}

	auth, err := kc.Resolve(registry(t, "ghcr.io"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if auth != authn.Anonymous {
		t.Fatalf("unmatched registry should fall through to anonymous, got %T", auth)
	}
}

func TestECRKeychainTokenCaching(t *testing.T) {
	fetches := 0
	fetch := func(ctx context.Context, region string) (string, string, time.Time, error) {
		fetches++
		if region != "us-east-1" {
			t.Errorf("fetch called with region %q, want us-east-1", region)
		}
		return "AWS", "token-password", time.Now().Add(12 * time.Hour), nil
	}

	k := &ecrKeychain{
		pattern:    "*.dkr.ecr.us-east-1.amazonaws.com",
		tokens:     make(map[string]*ecrToken),
		clients:    make(map[string]*ecr.Client),
		fetchToken: fetch,
	}

	host := "123456789012.dkr.ecr.us-east-1.amazonaws.com"
	for i := 0; i < 3; i++ {
		auth, err := k.Resolve(registry(t, host))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		cfg, err := auth.Authorization()
		if err != nil {
			t.Fatalf("Authorization: %v", err)
		}
		if cfg.Username != "AWS" || cfg.Password != "token-password" {
			t.Fatalf("got credentials %q/%q", cfg.Username, cfg.Password)
		}
	}
	if fetches != 1 {
		t.Fatalf("token was fetched %d times, want 1 (cached)", fetches)
	}
}

func TestECRKeychainRefreshNearExpiry(t *testing.T) {
	fetches := 0
	fetch := func(ctx context.Context, region string) (string, string, time.Time, error) {
		fetches++
		// First token expires inside the refresh margin.
		if fetches == 1 {
			return "AWS", "old", time.Now().Add(time.Minute), nil
		}
		return "AWS", "new", time.Now().Add(12 * time.Hour), nil
	}

	k := &ecrKeychain{
		pattern:    "*.dkr.ecr.us-east-1.amazonaws.com",
		tokens:     make(map[string]*ecrToken),
		clients:    make(map[string]*ecr.Client),
		fetchToken: fetch,
	}

	host := "123456789012.dkr.ecr.us-east-1.amazonaws.com"
	for i := 0; i < 2; i++ {
		auth, err := k.Resolve(registry(t, host))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		cfg, err := auth.Authorization()
		if err != nil {
			t.Fatalf("Authorization: %v", err)
		}
		if i == 1 && cfg.Password != "new" {
			t.Fatalf("expected refreshed token, got %q", cfg.Password)
		}
	}
	if fetches != 2 {
		t.Fatalf("token was fetched %d times, want 2 (refresh near expiry)", fetches)
	}
}

func TestECRKeychainMismatch(t *testing.T) {
	k := &ecrKeychain{
		pattern: "*.dkr.ecr.us-east-1.amazonaws.com",
		tokens:  make(map[string]*ecrToken),
		clients: make(map[string]*ecr.Client),
		fetchToken: func(ctx context.Context, region string) (string, string, time.Time, error) {
			return "", "", time.Time{}, errors.New("must not be called")
		},
	}

	// Different region: pattern does not match.
	auth, err := k.Resolve(registry(t, "123456789012.dkr.ecr.eu-west-1.amazonaws.com"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if auth != authn.Anonymous {
		t.Fatalf("non-matching host should be anonymous, got %T", auth)
	}

	// Pattern matches but the host is not a private ECR endpoint: no token
	// can be fetched, so fall through instead of failing.
	k.pattern = "*"
	auth, err = k.Resolve(registry(t, "docker.io"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if auth != authn.Anonymous {
		t.Fatalf("non-ECR host should be anonymous, got %T", auth)
	}
}
