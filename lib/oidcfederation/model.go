// Package oidcfederation validates externally issued OIDC identity tokens against
// operator-configured issuers and federation rules.
package oidcfederation

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const (
	ReplaySingleUse   = "single_use"
	ReplayAllowReuse  = "allow_reuse"
	GitHubIssuer      = "https://token.actions.githubusercontent.com"
	GoogleIssuer      = "https://accounts.google.com"
	DefaultTokenTTL   = 300
	MaxTokenTTL       = 600
	MaxAssertionBytes = 16 << 10
	// Azure managed identity tokens live slightly over 24 hours (exp minus iat
	// is about 86700 seconds), so the cap leaves room above a full day.
	MaxIncomingLifetimeSeconds = 48 * 60 * 60
)

var (
	ErrInvalid     = errors.New("invalid oidc federation configuration")
	ErrAssertion   = errors.New("invalid oidc assertion")
	ErrUnavailable = errors.New("oidc issuer keys unavailable")
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
	numericID      = regexp.MustCompile(`^[0-9]{1,32}$`)
	entraIssuer    = regexp.MustCompile(`^https://(?:sts\.windows\.net/([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})/|login\.microsoftonline\.com/([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})/v2\.0)$`)
	kubeLabel      = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
)

// KeySource selects one configured public verification-key source.
type KeySource struct {
	Type             string `koanf:"type" json:"type"`
	URL              string `koanf:"url" json:"url,omitempty"`
	DiscoveryBaseURL string `koanf:"discovery_base_url" json:"discovery_base_url,omitempty"`
	CACertPEM        string `koanf:"ca_cert_pem" json:"ca_cert_pem,omitempty"`
	// Keys holds public JWKs for the inline source.
	Keys []map[string]any `koanf:"keys" json:"keys,omitempty"`
}

// TokenValidation bounds incoming assertion lifetime and sets the replay policy.
type TokenValidation struct {
	MaxLifetimeSeconds *int   `koanf:"max_lifetime_seconds" json:"max_lifetime_seconds,omitempty"`
	ReplayProtection   string `koanf:"replay_protection" json:"replay_protection,omitempty"`
}

// IssuerSpec is the operator's explicit external trust configuration.
type IssuerSpec struct {
	Name            string          `koanf:"name" json:"name"`
	IssuerURL       string          `koanf:"issuer_url" json:"issuer_url"`
	PresetType      string          `koanf:"preset_type" json:"preset_type,omitempty"`
	JWKS            KeySource       `koanf:"jwks" json:"jwks"`
	TokenValidation TokenValidation `koanf:"token_validation" json:"token_validation"`
}

// Match contains conditions that must all hold on verified identity claims.
type Match struct {
	Type              string            `koanf:"type" json:"type"`
	Subject           string            `koanf:"subject" json:"subject,omitempty"`
	SubjectPrefix     string            `koanf:"subject_prefix" json:"subject_prefix,omitempty"`
	Claims            map[string]string `koanf:"claims" json:"claims,omitempty"`
	RepositoryID      string            `koanf:"repository_id" json:"repository_id,omitempty"`
	RepositoryOwnerID string            `koanf:"repository_owner_id" json:"repository_owner_id,omitempty"`
	Workflow          string            `koanf:"workflow" json:"workflow,omitempty"`
	WorkflowRef       string            `koanf:"workflow_ref" json:"workflow_ref,omitempty"`
	JobWorkflowRef    string            `koanf:"job_workflow_ref" json:"job_workflow_ref,omitempty"`
	Ref               string            `koanf:"ref" json:"ref,omitempty"`
	Environment       string            `koanf:"environment" json:"environment,omitempty"`
	EventNames        []string          `koanf:"event_names" json:"event_names,omitempty"`
	Namespace         string            `koanf:"namespace" json:"namespace,omitempty"`
	ServiceAccount    string            `koanf:"service_account" json:"service_account,omitempty"`
}

// RuleAuthorization is what a matching workload receives: a scope list and a token lifetime.
type RuleAuthorization struct {
	Permissions        []string `koanf:"permissions" json:"permissions"`
	MaxTokenTTLSeconds *int     `koanf:"max_token_ttl_seconds" json:"max_token_ttl_seconds,omitempty"`
}

// RuleSpec maps a matching external identity to a scoped Hypeman token.
type RuleSpec struct {
	Name             string            `koanf:"name" json:"name"`
	Issuer           string            `koanf:"issuer" json:"issuer"`
	ExpectedAudience string            `koanf:"expected_audience" json:"expected_audience"`
	Match            Match             `koanf:"match" json:"match"`
	Authorization    RuleAuthorization `koanf:"authorization" json:"authorization"`
}

// Config is the `oidc` block of the server configuration.
type Config struct {
	Issuers         []IssuerSpec `koanf:"issuers" json:"issuers"`
	FederationRules []RuleSpec   `koanf:"federation_rules" json:"federation_rules"`
}

// validateName enforces alphanumeric-and-hyphen names.
func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: name must contain only alphanumeric characters and hyphens", ErrInvalid)
	}
	return nil
}

// httpsURL validates endpoint syntax; the HTTP transport also validates its dial address.
func httpsURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(parsed.Hostname(), "%") {
		return nil, fmt.Errorf("%w: an HTTPS URL without credentials or fragment is required", ErrInvalid)
	}
	return parsed, nil
}
