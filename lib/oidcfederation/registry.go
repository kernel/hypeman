package oidcfederation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Rule is a validated federation rule together with the issuer it trusts.
type Rule struct {
	RuleSpec
	IssuerSpec IssuerSpec
	// Revision changes whenever the rule or its issuer changes, so tokens
	// issued under an earlier configuration can be told apart.
	Revision string
}

// Registry holds the validated issuers and rules of one configuration.
type Registry struct {
	rules map[string]Rule
}

// Compile validates the configuration and resolves each rule's issuer.
func Compile(cfg Config) (*Registry, error) {
	issuers := make(map[string]IssuerSpec, len(cfg.Issuers))
	urls := make(map[string]string, len(cfg.Issuers))
	for i, spec := range cfg.Issuers {
		issuer, err := normalizeIssuer(spec)
		if err != nil {
			return nil, fmt.Errorf("oidc.issuers[%d] (%q): %w", i, spec.Name, err)
		}
		if _, exists := issuers[issuer.Name]; exists {
			return nil, fmt.Errorf("oidc.issuers[%d] (%q): %w: duplicate issuer name", i, issuer.Name, ErrInvalid)
		}
		if other, exists := urls[issuer.IssuerURL]; exists {
			return nil, fmt.Errorf("oidc.issuers[%d] (%q): %w: issuer URL is already configured as %q", i, issuer.Name, ErrInvalid, other)
		}
		issuers[issuer.Name] = issuer
		urls[issuer.IssuerURL] = issuer.Name
	}
	rules := make(map[string]Rule, len(cfg.FederationRules))
	for i, spec := range cfg.FederationRules {
		issuer, exists := issuers[spec.Issuer]
		if !exists {
			return nil, fmt.Errorf("oidc.federation_rules[%d] (%q): %w: unknown issuer %q", i, spec.Name, ErrInvalid, spec.Issuer)
		}
		rule, err := normalizeRule(spec, issuer)
		if err != nil {
			return nil, fmt.Errorf("oidc.federation_rules[%d] (%q): %w", i, spec.Name, err)
		}
		if _, exists := rules[rule.Name]; exists {
			return nil, fmt.Errorf("oidc.federation_rules[%d] (%q): %w: duplicate rule name", i, rule.Name, ErrInvalid)
		}
		raw, err := json.Marshal(struct {
			Rule   RuleSpec
			Issuer IssuerSpec
		}{rule, issuer})
		if err != nil {
			return nil, fmt.Errorf("oidc.federation_rules[%d] (%q): %w", i, spec.Name, ErrInvalid)
		}
		digest := sha256.Sum256(raw)
		rules[rule.Name] = Rule{RuleSpec: rule, IssuerSpec: issuer, Revision: hex.EncodeToString(digest[:8])}
	}
	return &Registry{rules: rules}, nil
}

// Rule returns the named federation rule.
func (r *Registry) Rule(name string) (Rule, bool) {
	if r == nil {
		return Rule{}, false
	}
	rule, ok := r.rules[name]
	return rule, ok
}

// Enabled reports whether any federation rule is configured.
func (r *Registry) Enabled() bool {
	return r != nil && len(r.rules) > 0
}
