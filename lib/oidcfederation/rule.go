package oidcfederation

import (
	"fmt"
	"slices"
	"time"

	"github.com/kernel/hypeman/lib/scopes"
)

// normalizeRule validates a rule against its already normalized issuer.
func normalizeRule(out RuleSpec, issuer IssuerSpec) (RuleSpec, error) {
	if out.Match.EventNames != nil && len(out.Match.EventNames) == 0 {
		return out, fmt.Errorf("%w: event restrictions cannot be empty", ErrInvalid)
	}
	if err := validateName(out.Name); err != nil {
		return out, err
	}
	if out.ExpectedAudience == "" || len(out.ExpectedAudience) > 2048 {
		return out, fmt.Errorf("%w: expected_audience is required", ErrInvalid)
	}
	if out.Authorization.MaxTokenTTLSeconds == nil {
		n := DefaultTokenTTL
		out.Authorization.MaxTokenTTLSeconds = &n
	}
	if n := *out.Authorization.MaxTokenTTLSeconds; n < 1 || n > MaxTokenTTL {
		return out, fmt.Errorf("%w: token lifetime must be between 1 and %d seconds", ErrInvalid, MaxTokenTTL)
	}
	if len(out.Authorization.Audiences) == 0 {
		return out, fmt.Errorf("%w: authorization.audiences is required", ErrInvalid)
	}
	out.Authorization.Audiences = slices.Clone(out.Authorization.Audiences)
	for i, audience := range out.Authorization.Audiences {
		if audience == "" || len(audience) > 2048 || slices.Contains(out.Authorization.Audiences[:i], audience) {
			return out, fmt.Errorf("%w: authorization.audiences[%d] is empty, too long or repeated", ErrInvalid, i)
		}
	}
	if out.Authorization.SubjectClaim != "" {
		if _, err := pointerParts(out.Authorization.SubjectClaim); err != nil {
			return out, fmt.Errorf("%w: authorization.subject_claim must be a JSON Pointer", ErrInvalid)
		}
	}
	// A federated token never falls back to the full access a token without a
	// permissions claim has, so the list is explicit and has no wildcard.
	if len(out.Authorization.Permissions) == 0 {
		return out, fmt.Errorf("%w: authorization.permissions is required", ErrInvalid)
	}
	out.Authorization.Permissions = slices.Clone(out.Authorization.Permissions)
	slices.Sort(out.Authorization.Permissions)
	if len(slices.Compact(slices.Clone(out.Authorization.Permissions))) != len(out.Authorization.Permissions) {
		return out, fmt.Errorf("%w: duplicate permission", ErrInvalid)
	}
	for _, permission := range out.Authorization.Permissions {
		if scope := scopes.Scope(permission); scope == scopes.All || !scope.Valid() {
			return out, fmt.Errorf("%w: unsupported permission %q", ErrInvalid, permission)
		}
	}
	out.Match.Claims = cloneClaims(out.Match.Claims)
	out.Match.EventNames = slices.Clone(out.Match.EventNames)
	if err := normalizeMatch(&out.Match, issuer); err != nil {
		return out, err
	}
	if out.Match.Type == "github_actions" && out.Match.Ref == "" && out.Match.Environment == "" {
		return out, fmt.Errorf("%w: GitHub requires a branch or environment restriction", ErrInvalid)
	}
	return out, nil
}

func cloneClaims(claims map[string]string) map[string]string {
	if claims == nil {
		return nil
	}
	out := make(map[string]string, len(claims))
	for key, value := range claims {
		out[key] = value
	}
	return out
}

// AccessExpiry clips an issued token's lifetime to the assertion's own expiry.
func AccessExpiry(now, assertionExpiry time.Time, ttlSeconds int) (time.Time, error) {
	if ttlSeconds < 1 || ttlSeconds > MaxTokenTTL {
		return time.Time{}, ErrInvalid
	}
	expiry := now.Add(time.Duration(ttlSeconds) * time.Second).Truncate(time.Second)
	if !assertionExpiry.IsZero() && assertionExpiry.Before(expiry) {
		expiry = assertionExpiry.Truncate(time.Second)
	}
	if assertionExpiry.IsZero() || !expiry.After(now) {
		return time.Time{}, ErrAssertion
	}
	return expiry, nil
}
