package oidcfederation

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

var presetTypes = []string{"github_actions", "aws", "google_cloud", "microsoft_entra", "kubernetes", "custom_oidc"}

// normalizeIssuer derives preset defaults and validates supported key and assertion settings.
func normalizeIssuer(out IssuerSpec) (IssuerSpec, error) {
	if err := validateName(out.Name); err != nil {
		return out, err
	}
	switch out.PresetType {
	case "github_actions":
		if out.IssuerURL == "" {
			out.IssuerURL = GitHubIssuer
		}
	case "google_cloud":
		if out.IssuerURL == "" {
			out.IssuerURL = GoogleIssuer
		}
	}
	issuerURL, err := httpsURL(out.IssuerURL)
	if err != nil || issuerURL.RawQuery != "" {
		return out, fmt.Errorf("%w: invalid issuer URL", ErrInvalid)
	}
	known := presetForIssuer(out.IssuerURL)
	if out.PresetType == "" {
		out.PresetType = cmp.Or(known, "custom_oidc")
	}
	if !slices.Contains(presetTypes, out.PresetType) {
		return out, fmt.Errorf("%w: unsupported issuer preset", ErrInvalid)
	}
	// GitHub, Google and Entra issuers are recognized by URL, so no other preset can relax their rules.
	if known != out.PresetType && (known != "" || slices.Contains([]string{"github_actions", "google_cloud", "microsoft_entra"}, out.PresetType)) {
		return out, fmt.Errorf("%w: preset and issuer URL disagree", ErrInvalid)
	}
	if out.JWKS.Type == "" {
		out.JWKS.Type = "discovery"
	}
	if out.TokenValidation.MaxLifetimeSeconds == nil {
		v := 3600
		if out.PresetType == "microsoft_entra" {
			v = 7500
		}
		out.TokenValidation.MaxLifetimeSeconds = &v
	}
	if n := *out.TokenValidation.MaxLifetimeSeconds; n < 1 || n > MaxIncomingLifetimeSeconds {
		return out, fmt.Errorf("%w: incoming lifetime must be between 1 and %d seconds", ErrInvalid, MaxIncomingLifetimeSeconds)
	}
	if out.TokenValidation.ReplayProtection == "" {
		out.TokenValidation.ReplayProtection = ReplaySingleUse
		// A pod reads the same projected token until the kubelet rotates it, so
		// single use would fail every exchange after the first.
		if out.PresetType == "google_cloud" || out.PresetType == "kubernetes" {
			out.TokenValidation.ReplayProtection = ReplayAllowReuse
		}
	}
	if !slices.Contains([]string{ReplaySingleUse, ReplayAllowReuse}, out.TokenValidation.ReplayProtection) {
		return out, fmt.Errorf("%w: unsupported replay policy", ErrInvalid)
	}
	if out.TokenValidation.ReplayProtection == ReplayAllowReuse && !slices.Contains([]string{"google_cloud", "microsoft_entra", "kubernetes", "custom_oidc"}, out.PresetType) {
		return out, fmt.Errorf("%w: GitHub and AWS assertions are always single-use", ErrInvalid)
	}
	// A Google identity token carries no token ID, so there is nothing to consume.
	if out.TokenValidation.ReplayProtection == ReplaySingleUse && out.PresetType == "google_cloud" {
		return out, fmt.Errorf("%w: Google identity tokens carry no token ID and require allow_reuse", ErrInvalid)
	}
	if err := validateCA(out.JWKS.CACertPEM); err != nil {
		return out, err
	}
	switch out.JWKS.Type {
	case "discovery":
		if out.JWKS.URL != "" || len(out.JWKS.Keys) != 0 {
			return out, fmt.Errorf("%w: conflicting key sources", ErrInvalid)
		}
		if strings.HasPrefix(out.IssuerURL, "https://sts.windows.net/") && out.PresetType == "microsoft_entra" && out.JWKS.DiscoveryBaseURL == "" {
			out.JWKS.DiscoveryBaseURL = "https://login.microsoftonline.com/" + entraTenant(out.IssuerURL)
		}
		if out.JWKS.DiscoveryBaseURL != "" {
			base, err := httpsURL(out.JWKS.DiscoveryBaseURL)
			if err != nil || base.RawQuery != "" {
				return out, fmt.Errorf("%w: invalid discovery base URL", ErrInvalid)
			}
		}
	case "explicit_url":
		if out.JWKS.DiscoveryBaseURL != "" || len(out.JWKS.Keys) != 0 {
			return out, fmt.Errorf("%w: conflicting key sources", ErrInvalid)
		}
		if _, err := httpsURL(out.JWKS.URL); err != nil {
			return out, err
		}
	case "inline":
		if out.JWKS.URL != "" || out.JWKS.DiscoveryBaseURL != "" || out.JWKS.CACertPEM != "" || len(out.JWKS.Keys) == 0 {
			return out, fmt.Errorf("%w: conflicting or missing inline keys", ErrInvalid)
		}
		raw, err := json.Marshal(map[string]any{"keys": out.JWKS.Keys})
		if err != nil {
			return out, fmt.Errorf("%w: invalid public keys", ErrInvalid)
		}
		keys, err := parseKeySet(raw)
		if err != nil {
			return out, fmt.Errorf("%w: invalid public keys", ErrInvalid)
		}
		// Keep only the signing keys and public fields the parser accepted, in
		// a new slice: decoding into the configured one would rewrite the
		// caller's entries in place.
		var accepted []map[string]any
		raw, err = json.Marshal(keys)
		if err != nil || json.Unmarshal(raw, &accepted) != nil {
			return out, fmt.Errorf("%w: invalid public keys", ErrInvalid)
		}
		out.JWKS.Keys = accepted
	default:
		return out, fmt.Errorf("%w: unsupported key source", ErrInvalid)
	}
	if out.IssuerURL == GitHubIssuer && (out.TokenValidation.ReplayProtection != ReplaySingleUse || out.JWKS.Type != "discovery" || out.JWKS.DiscoveryBaseURL != "" || out.JWKS.CACertPEM != "") {
		return out, fmt.Errorf("%w: GitHub requires its canonical discovery and single-use assertions", ErrInvalid)
	}
	return out, nil
}

// AuthorizationServer describes an OAuth authorization server whose access
// tokens a resource server verifies. It differs from an identity issuer in two
// ways: its metadata is read from the RFC 8414 location, and its last key set
// stays in use while a refresh fails, because every request to the resource
// server depends on those keys.
func AuthorizationServer(issuerURL string, keys KeySource) (IssuerSpec, error) {
	spec, err := normalizeIssuer(IssuerSpec{Name: "authorization-server", IssuerURL: issuerURL, PresetType: "custom_oidc", JWKS: keys})
	if err != nil {
		return spec, err
	}
	if spec.JWKS.DiscoveryBaseURL != "" {
		return spec, fmt.Errorf("%w: an authorization server publishes its own metadata", ErrInvalid)
	}
	spec.authorizationServer = true
	return spec, nil
}

// entraTenant returns the tenant an Entra v1 or v2 issuer URL names.
func entraTenant(issuerURL string) string {
	groups := entraIssuer.FindStringSubmatch(issuerURL)
	if groups == nil {
		return ""
	}
	return cmp.Or(groups[1], groups[2])
}

// presetForIssuer returns the preset an issuer URL belongs to, if any.
func presetForIssuer(issuerURL string) string {
	switch {
	case issuerURL == GitHubIssuer:
		return "github_actions"
	case issuerURL == GoogleIssuer:
		return "google_cloud"
	case entraTenant(issuerURL) != "":
		return "microsoft_entra"
	}
	return ""
}
