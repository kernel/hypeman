// Package registryauth describes credentials borrowed by a single build.
package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

const MaxBytes = 64 * 1024

var ErrInvalid = errors.New("invalid registry_credentials: use registry hosts and nonempty username/password pairs with a future expires_at")

type Credential struct {
	Username  string     `json:"username"`
	Password  string     `json:"password"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// String prevents accidental formatting of a credential from disclosing its value.
func (Credential) String() string { return "[registry credential]" }

// Normalize validates hosts, rejects local-registry overrides, and copies caller-owned values.
func Normalize(credentials map[string]Credential, localRegistry string, now time.Time) (map[string]Credential, error) {
	if len(credentials) == 0 {
		return make(map[string]Credential), nil
	}
	if len(credentials) > 16 {
		return nil, ErrInvalid
	}
	localRegistry = strings.TrimPrefix(strings.TrimPrefix(localRegistry, "https://"), "http://")
	local, err := name.NewRegistry(strings.ToLower(localRegistry))
	if err != nil {
		return nil, ErrInvalid
	}
	out := make(map[string]Credential, len(credentials))
	size := 0
	for host, credential := range credentials {
		size += len(host) + len(credential.Username) + len(credential.Password)
		u, err := url.Parse("https://" + host)
		if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(host, " \t\r\n") {
			return nil, ErrInvalid
		}
		registry, err := name.NewRegistry(strings.ToLower(host))
		if err != nil || registry.Name() == local.Name() || size > MaxBytes || credential.Username == "" || credential.Password == "" || strings.ContainsAny(credential.Username+credential.Password, "\r\n") {
			return nil, ErrInvalid
		}
		if credential.ExpiresAt != nil {
			expires := *credential.ExpiresAt
			if !expires.After(now) {
				return nil, ErrInvalid
			}
			credential.ExpiresAt = &expires
		}
		if _, exists := out[registry.Name()]; exists {
			return nil, ErrInvalid
		}
		out[registry.Name()] = credential
	}
	return out, nil
}

func DockerKey(host string) string {
	if host == name.DefaultRegistry {
		return "https://index.docker.io/v1/"
	}
	return host
}

func (c Credential) Auth() string {
	return base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
}

// Redactor covers raw, JSON-escaped, URL-encoded, and Docker-config representations.
func Redactor(credentials map[string]Credential) func(string) string {
	values := make(map[string]bool)
	for _, c := range credentials {
		quoted, _ := json.Marshal(c.Password)
		for _, value := range []string{c.Password, c.Auth(), base64.StdEncoding.EncodeToString([]byte(c.Password)), string(quoted[1 : len(quoted)-1]), url.QueryEscape(c.Password), url.PathEscape(c.Password)} {
			if value != "" {
				values[value] = true
			}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	pairs := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		pairs = append(pairs, key, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...).Replace
}
