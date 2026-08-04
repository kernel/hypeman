package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
)

// ecrTokenRefreshMargin refreshes cached ECR tokens this long before their
// server-side expiry so an in-flight pull never races the deadline.
const ecrTokenRefreshMargin = 5 * time.Minute

// ecrHostPattern extracts the region from a private ECR registry host, e.g.
// "123456789012.dkr.ecr.us-east-1.amazonaws.com" -> "us-east-1". Covers the
// -fips endpoints as well.
var ecrHostPattern = regexp.MustCompile(`\.dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com$`)

// ecrKeychain fetches short-lived pull tokens from AWS ECR and caches them
// per region until shortly before expiry.
type ecrKeychain struct {
	pattern         string
	accessKeyID     string
	secretAccessKey string

	// fetchToken returns (username, password, expiresAt) for a region.
	// Tests replace it; production uses GetAuthorizationToken.
	fetchToken func(ctx context.Context, region string) (string, string, time.Time, error)

	// mu guards tokens; the fetch itself runs under it so concurrent
	// pulls share one refresh per region.
	mu     sync.Mutex
	tokens map[string]*ecrToken

	clientMu sync.Mutex
	clients  map[string]*ecr.Client
}

type ecrToken struct {
	username  string
	password  string
	expiresAt time.Time
}

func newECRKeychain(cfg RegistryConfig) *ecrKeychain {
	k := &ecrKeychain{
		pattern:         cfg.Host,
		accessKeyID:     cfg.AccessKeyID,
		secretAccessKey: cfg.SecretAccessKey,
		tokens:          make(map[string]*ecrToken),
		clients:         make(map[string]*ecr.Client),
	}
	k.fetchToken = k.fetchTokenFromAWS
	return k
}

func (k *ecrKeychain) Resolve(reg authn.Resource) (authn.Authenticator, error) {
	host := reg.RegistryStr()
	if !matchHost(k.pattern, host) {
		return authn.Anonymous, nil
	}
	region := ecrRegionFromHost(host)
	if region == "" {
		// Pattern matched but the host is not a private ECR endpoint, so
		// there is no token to fetch; let later keychains handle it.
		return authn.Anonymous, nil
	}

	username, password, err := k.token(context.Background(), region)
	if err != nil {
		return nil, fmt.Errorf("get ECR token for %s: %w", host, err)
	}
	return authn.FromConfig(authn.AuthConfig{
		Username: username,
		Password: password,
	}), nil
}

// token returns a cached ECR token for the region, fetching a fresh one
// when the cache is empty or near expiry.
func (k *ecrKeychain) token(ctx context.Context, region string) (string, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if t, ok := k.tokens[region]; ok && time.Now().Add(ecrTokenRefreshMargin).Before(t.expiresAt) {
		return t.username, t.password, nil
	}

	username, password, expiresAt, err := k.fetchToken(ctx, region)
	if err != nil {
		return "", "", err
	}
	k.tokens[region] = &ecrToken{username: username, password: password, expiresAt: expiresAt}
	return username, password, nil
}

// ecrRegionFromHost extracts the AWS region from a private ECR registry
// host, or returns "" for anything else.
func ecrRegionFromHost(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	m := ecrHostPattern.FindStringSubmatch(host)
	if m == nil {
		return ""
	}
	return m[1]
}

func (k *ecrKeychain) fetchTokenFromAWS(ctx context.Context, region string) (string, string, time.Time, error) {
	client, err := k.clientForRegion(region)
	if err != nil {
		return "", "", time.Time{}, err
	}

	out, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", "", time.Time{}, err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", "", time.Time{}, errors.New("no authorization data in ECR response")
	}

	data := out.AuthorizationData[0]
	decoded, err := base64.StdEncoding.DecodeString(*data.AuthorizationToken)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("decode ECR authorization token: %w", err)
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", time.Time{}, errors.New("ECR authorization token is not in user:password form")
	}
	expiresAt := time.Now().Add(12 * time.Hour)
	if data.ExpiresAt != nil {
		expiresAt = *data.ExpiresAt
	}
	return username, password, expiresAt, nil
}

func (k *ecrKeychain) clientForRegion(region string) (*ecr.Client, error) {
	k.clientMu.Lock()
	defer k.clientMu.Unlock()

	if client, ok := k.clients[region]; ok {
		return client, nil
	}

	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if k.accessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(k.accessKeyID, k.secretAccessKey, ""),
		))
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	client := ecr.NewFromConfig(cfg)
	k.clients[region] = client
	return client, nil
}
