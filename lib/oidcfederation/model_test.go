package oidcfederation

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func githubConfig() Config {
	return Config{
		Issuers: []IssuerSpec{{Name: "github-actions", PresetType: "github_actions"}},
		FederationRules: []RuleSpec{{
			Name:             "ci",
			Issuer:           "github-actions",
			ExpectedAudience: "https://auth.example.test",
			Match:            Match{Type: "github_actions", RepositoryID: "12", RepositoryOwnerID: "34", Workflow: ".github/workflows/test.yml", Ref: "refs/heads/main"},
			Authorization:    RuleAuthorization{Permissions: []string{"instance:write", "instance:read"}},
		}},
	}
}

func githubRule(t *testing.T) Rule {
	t.Helper()
	registry, err := Compile(githubConfig())
	require.NoError(t, err)
	rule, ok := registry.Rule("ci")
	require.True(t, ok)
	return rule
}

func githubClaims() map[string]any {
	return map[string]any{
		"sub":                 "repo:renamed/tests:environment:staging",
		"repository":          "renamed/tests",
		"repository_id":       "12",
		"repository_owner_id": "34",
		"workflow_ref":        "renamed/tests/.github/workflows/test.yml@refs/heads/main",
		"ref":                 "refs/heads/main",
		"event_name":          "push",
		"environment":         "staging",
	}
}

// ruleFor compiles one rule against one issuer, as a configuration would.
func ruleFor(issuer IssuerSpec, match Match) (Rule, error) {
	registry, err := Compile(Config{
		Issuers: []IssuerSpec{issuer},
		FederationRules: []RuleSpec{{
			Name: "rule", Issuer: issuer.Name, ExpectedAudience: "https://auth.example.test", Match: match,
			Authorization: RuleAuthorization{Permissions: []string{"instance:read"}},
		}},
	})
	if err != nil {
		return Rule{}, err
	}
	rule, _ := registry.Rule("rule")
	return rule, nil
}

func TestIssuerPresetNormalization(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	for _, test := range []struct {
		name, given, issuer, preset, want string
	}{
		{"github", "", GitHubIssuer, "github_actions", "github_actions"},
		{"github by url", GitHubIssuer, GitHubIssuer, "", "github_actions"},
		{"google", "", GoogleIssuer, "google_cloud", "google_cloud"},
		{"aws", "https://sts.example.test/account", "https://sts.example.test/account", "aws", "aws"},
		{"entra-v1", "https://sts.windows.net/" + tenant + "/", "https://sts.windows.net/" + tenant + "/", "microsoft_entra", "microsoft_entra"},
		{"entra-v2 by url", "https://login.microsoftonline.com/" + tenant + "/v2.0", "https://login.microsoftonline.com/" + tenant + "/v2.0", "", "microsoft_entra"},
		{"kubernetes", "https://cluster.example.test", "https://cluster.example.test", "kubernetes", "kubernetes"},
		{"custom by default", "https://identity.example.test/path", "https://identity.example.test/path", "", "custom_oidc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := normalizeIssuer(IssuerSpec{Name: "issuer", IssuerURL: test.given, PresetType: test.preset})
			require.NoError(t, err)
			require.Equal(t, test.issuer, out.IssuerURL)
			require.Equal(t, test.want, out.PresetType)
			replay := ReplaySingleUse
			if test.name == "google" || test.name == "kubernetes" {
				replay = ReplayAllowReuse
			}
			require.Equal(t, replay, out.TokenValidation.ReplayProtection)
			if test.name == "entra-v1" {
				require.Equal(t, "https://login.microsoftonline.com/"+tenant, out.JWKS.DiscoveryBaseURL)
				require.Equal(t, 7500, *out.TokenValidation.MaxLifetimeSeconds)
			}
		})
	}
}

func TestIssuerRejectsConflictingSettings(t *testing.T) {
	for _, input := range []IssuerSpec{
		{Name: "bad_name", IssuerURL: "https://identity.example.test"},
		{Name: "issuer", IssuerURL: "https://wrong.example.test", PresetType: "github_actions"},
		{Name: "issuer", IssuerURL: "https://identity.example.test", PresetType: "microsoft_entra"},
		{Name: "issuer", IssuerURL: "https://identity.example.test", PresetType: "unknown"},
		{Name: "issuer", PresetType: "aws"},
		{Name: "issuer", IssuerURL: GitHubIssuer, PresetType: "custom_oidc"},
		{Name: "issuer", IssuerURL: GoogleIssuer, PresetType: "kubernetes", TokenValidation: TokenValidation{ReplayProtection: ReplayAllowReuse}},
		{Name: "issuer", PresetType: "google_cloud", TokenValidation: TokenValidation{ReplayProtection: ReplaySingleUse}},
		{Name: "issuer", PresetType: "github_actions", TokenValidation: TokenValidation{ReplayProtection: ReplayAllowReuse}},
		{Name: "issuer", IssuerURL: "https://sts.example.test/account", PresetType: "aws", TokenValidation: TokenValidation{ReplayProtection: ReplayAllowReuse}},
		{Name: "issuer", IssuerURL: GitHubIssuer, JWKS: KeySource{Type: "explicit_url", URL: "https://keys.example.test"}},
		{Name: "issuer", IssuerURL: "https://identity.example.test", JWKS: KeySource{Type: "inline"}},
		{Name: "issuer", IssuerURL: "https://identity.example.test", JWKS: KeySource{Type: "inline", Keys: []map[string]any{{"kty": "oct", "k": "c2VjcmV0"}}}},
		{Name: "issuer", IssuerURL: "https://identity.example.test", JWKS: KeySource{Type: "discovery", URL: "https://keys.example.test"}},
		{Name: "issuer", IssuerURL: "https://user:password@identity.example.test"},
		{Name: "issuer", IssuerURL: "http://identity.example.test"},
	} {
		_, err := normalizeIssuer(input)
		require.ErrorIs(t, err, ErrInvalid, "%+v", input)
	}
}

func TestIssuerIncomingLifetimeBounds(t *testing.T) {
	for limit, ok := range map[int]bool{0: false, 1: true, 86400: true, 86700: true, MaxIncomingLifetimeSeconds: true, MaxIncomingLifetimeSeconds + 1: false} {
		_, err := normalizeIssuer(IssuerSpec{Name: "issuer", IssuerURL: "https://identity.example.test", TokenValidation: TokenValidation{MaxLifetimeSeconds: &limit}})
		if ok {
			require.NoError(t, err, limit)
		} else {
			require.ErrorIs(t, err, ErrInvalid, limit)
		}
	}
}

// A pod reads the same projected token until it is rotated, so reuse is the
// Kubernetes default. A workload that fetches a fresh token each time can still
// ask for single use.
func TestKubernetesAllowsReuseUnlessToldOtherwise(t *testing.T) {
	spec := IssuerSpec{Name: "cluster", IssuerURL: "https://cluster.example.test", PresetType: "kubernetes"}
	out, err := normalizeIssuer(spec)
	require.NoError(t, err)
	require.Equal(t, ReplayAllowReuse, out.TokenValidation.ReplayProtection)
	spec.TokenValidation.ReplayProtection = ReplaySingleUse
	out, err = normalizeIssuer(spec)
	require.NoError(t, err)
	require.Equal(t, ReplaySingleUse, out.TokenValidation.ReplayProtection)
}

// A generic matcher on a shared provider would accept every customer of that provider.
func TestRuleMatcherMustBeTheIssuersType(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	for _, test := range []struct {
		issuer IssuerSpec
		match  Match
		ok     bool
	}{
		{IssuerSpec{Name: "google", PresetType: "google_cloud"}, Match{Type: "google_cloud", Subject: "107517467455664443765"}, true},
		{IssuerSpec{Name: "google", PresetType: "google_cloud"}, Match{Type: "custom_oidc", Claims: map[string]string{"/iss": GoogleIssuer}}, false},
		{IssuerSpec{Name: "google", PresetType: "google_cloud"}, Match{Type: "aws", SubjectPrefix: "1"}, false},
		{IssuerSpec{Name: "entra", IssuerURL: "https://login.microsoftonline.com/" + tenant + "/v2.0"}, Match{Type: "microsoft_entra", Subject: "workload"}, true},
		{IssuerSpec{Name: "entra", IssuerURL: "https://login.microsoftonline.com/" + tenant + "/v2.0"}, Match{Type: "custom_oidc", Subject: "workload"}, false},
		{IssuerSpec{Name: "cluster", IssuerURL: "https://cluster.example.test", PresetType: "kubernetes"}, Match{Type: "custom_oidc", SubjectPrefix: "system:serviceaccount:"}, false},
		{IssuerSpec{Name: "cluster", IssuerURL: "https://cluster.example.test", PresetType: "kubernetes"}, Match{Type: "kubernetes", Namespace: "ci", ServiceAccount: "runner"}, true},
		{IssuerSpec{Name: "custom", IssuerURL: "https://identity.example.test"}, Match{Type: "google_cloud", Subject: "107517467455664443765"}, false},
		{IssuerSpec{Name: "custom", IssuerURL: "https://identity.example.test"}, Match{Type: "custom_oidc", Subject: "build"}, true},
		{IssuerSpec{Name: "custom", IssuerURL: "https://identity.example.test"}, Match{Type: "custom_oidc"}, false},
	} {
		_, err := ruleFor(test.issuer, test.match)
		if test.ok {
			require.NoError(t, err, "%s issuer with a %s matcher", test.issuer.Name, test.match.Type)
		} else {
			require.ErrorIs(t, err, ErrInvalid, "%s issuer with a %s matcher", test.issuer.Name, test.match.Type)
		}
	}
}

// A client ID names the app a token was issued through. A user signed in through
// that app gets a token with the same client ID, so it must not stand for the workload.
func TestEntraRulesAcceptOnlyTheApplicationsOwnTokens(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	issuer := IssuerSpec{Name: "entra", IssuerURL: "https://login.microsoftonline.com/" + tenant + "/v2.0"}
	app := map[string]any{"sub": "sp-object", "oid": "sp-object", "tid": tenant, "azp": "client-a", "idtyp": "app"}
	user := map[string]any{"sub": "pairwise-user", "oid": "user-object", "tid": tenant, "azp": "client-a", "scp": "access_as_user"}

	byClient, err := ruleFor(issuer, Match{Type: "microsoft_entra", Claims: map[string]string{"/azp": "client-a"}})
	require.NoError(t, err)
	require.Equal(t, "app", byClient.Match.Claims["/idtyp"], "a rule matched by client ID requires the app marker")
	require.Equal(t, tenant, byClient.Match.Claims["/tid"])
	require.True(t, byClient.Match.Matches(app))
	require.False(t, byClient.Match.Matches(user))
	unmarked := maps.Clone(app)
	delete(unmarked, "idtyp")
	require.False(t, byClient.Match.Matches(unmarked), "without the marker a client ID does not identify the workload")
	_, err = ruleFor(issuer, Match{Type: "microsoft_entra", Claims: map[string]string{"/azp": "client-a", "/idtyp": "user"}})
	require.ErrorIs(t, err, ErrInvalid)

	byObject, err := ruleFor(issuer, Match{Type: "microsoft_entra", Claims: map[string]string{"/oid": "sp-object"}})
	require.NoError(t, err)
	require.NotContains(t, byObject.Match.Claims, "/idtyp")
	require.True(t, byObject.Match.Matches(unmarked))
	delegated := maps.Clone(unmarked)
	delegated["scp"] = "access_as_user"
	require.False(t, byObject.Match.Matches(delegated), "a delegated token never stands for a workload")
}

func TestGitHubConditionsAreANDedAndNamesDoNotRebind(t *testing.T) {
	rule := githubRule(t)
	claims := githubClaims()
	require.True(t, rule.Match.Matches(claims), "immutable IDs permit environment-based subjects and survive a rename")
	for name, value := range map[string]any{
		"repository_id":       "99",
		"repository_owner_id": "99",
		"workflow_ref":        "another/tests/.github/workflows/test.yml@refs/heads/main",
		"ref":                 "refs/heads/other",
		"event_name":          "pull_request_target",
	} {
		changed := maps.Clone(claims)
		changed[name] = value
		require.False(t, rule.Match.Matches(changed), name)
	}
	delete(claims, "event_name")
	require.False(t, rule.Match.Matches(claims))
	rule.Match.JobWorkflowRef = "central/ci/.github/workflows/run.yml@refs/heads/main"
	claims = githubClaims()
	require.False(t, rule.Match.Matches(claims))
	claims["job_workflow_ref"] = rule.Match.JobWorkflowRef
	require.True(t, rule.Match.Matches(claims))
}

// A repository name is pinned like any other claim. There is no separate field
// for it that a rule could set and the matcher ignore.
func TestGitHubRepositoryNameIsPinnedByClaim(t *testing.T) {
	cfg := githubConfig()
	cfg.FederationRules[0].Match.Claims = map[string]string{"/repository": "renamed/tests"}
	registry, err := Compile(cfg)
	require.NoError(t, err)
	rule, _ := registry.Rule("ci")
	claims := githubClaims()
	require.True(t, rule.Match.Matches(claims))
	claims["repository"] = "other/tests"
	require.False(t, rule.Match.Matches(claims))
}

func TestGitHubRestrictionsCannotBeWeakened(t *testing.T) {
	for name, alter := range map[string]func(*Match){
		"generic matcher":    func(m *Match) { m.Type = "custom_oidc" },
		"no repository id":   func(m *Match) { m.RepositoryID = "" },
		"no owner id":        func(m *Match) { m.RepositoryOwnerID = "" },
		"no workflow":        func(m *Match) { m.Workflow = "" },
		"no ref":             func(m *Match) { m.Ref = "" },
		"empty event list":   func(m *Match) { m.EventNames = []string{} },
		"workflow traversal": func(m *Match) { m.Workflow = ".github/workflows/../../other.yml" },
	} {
		cfg := githubConfig()
		alter(&cfg.FederationRules[0].Match)
		_, err := Compile(cfg)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestRulePermissionsAreExplicit(t *testing.T) {
	rule := githubRule(t)
	require.Equal(t, []string{"instance:read", "instance:write"}, rule.Authorization.Permissions)
	require.Equal(t, DefaultTokenTTL, *rule.Authorization.MaxTokenTTLSeconds)
	require.Equal(t, []string{"push", "workflow_dispatch"}, rule.Match.EventNames)
	for name, permissions := range map[string][]string{
		"none":      nil,
		"wildcard":  {"*"},
		"unknown":   {"instance:admin"},
		"duplicate": {"instance:read", "instance:read"},
	} {
		cfg := githubConfig()
		cfg.FederationRules[0].Authorization.Permissions = permissions
		_, err := Compile(cfg)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	for _, ttl := range []int{0, MaxTokenTTL + 1} {
		cfg := githubConfig()
		cfg.FederationRules[0].Authorization.MaxTokenTTLSeconds = &ttl
		_, err := Compile(cfg)
		require.ErrorIs(t, err, ErrInvalid, ttl)
	}
}

func TestCompileResolvesIssuersAndRejectsAmbiguity(t *testing.T) {
	empty, err := Compile(Config{})
	require.NoError(t, err)
	require.False(t, empty.Enabled())
	_, ok := empty.Rule("ci")
	require.False(t, ok)
	var none *Registry
	require.False(t, none.Enabled())

	registry, err := Compile(githubConfig())
	require.NoError(t, err)
	require.True(t, registry.Enabled())

	for name, alter := range map[string]func(*Config){
		"unknown issuer":       func(c *Config) { c.FederationRules[0].Issuer = "other" },
		"duplicate rule":       func(c *Config) { c.FederationRules = append(c.FederationRules, c.FederationRules[0]) },
		"duplicate issuer":     func(c *Config) { c.Issuers = append(c.Issuers, c.Issuers[0]) },
		"duplicate issuer url": func(c *Config) { c.Issuers = append(c.Issuers, IssuerSpec{Name: "second", IssuerURL: GitHubIssuer}) },
		"no audience":          func(c *Config) { c.FederationRules[0].ExpectedAudience = "" },
		"bad rule name":        func(c *Config) { c.FederationRules[0].Name = "ci:main" },
	} {
		cfg := githubConfig()
		alter(&cfg)
		_, err := Compile(cfg)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

// A token issued under one configuration must not survive a change to its rule or issuer.
func TestRevisionFollowsRuleAndIssuer(t *testing.T) {
	base := githubRule(t).Revision
	require.Len(t, base, 16)
	require.Equal(t, base, githubRule(t).Revision, "the same configuration keeps its revision")
	for name, alter := range map[string]func(*Config){
		"permissions": func(c *Config) { c.FederationRules[0].Authorization.Permissions = []string{"instance:read"} },
		"match":       func(c *Config) { c.FederationRules[0].Match.Ref = "refs/heads/release" },
		"audience":    func(c *Config) { c.FederationRules[0].ExpectedAudience = "https://other.example.test" },
		"issuer": func(c *Config) {
			limit := 600
			c.Issuers[0].TokenValidation.MaxLifetimeSeconds = &limit
		},
	} {
		cfg := githubConfig()
		alter(&cfg)
		registry, err := Compile(cfg)
		require.NoError(t, err, name)
		rule, _ := registry.Rule("ci")
		require.NotEqual(t, base, rule.Revision, name)
	}
}

func TestCompileDoesNotAliasItsInput(t *testing.T) {
	cfg := githubConfig()
	cfg.FederationRules[0].Match.Claims = map[string]string{"/actor": "octocat"}
	registry, err := Compile(cfg)
	require.NoError(t, err)
	cfg.FederationRules[0].Match.Claims["/actor"] = "other"
	cfg.FederationRules[0].Authorization.Permissions[0] = "image:delete"
	rule, _ := registry.Rule("ci")
	require.Equal(t, "octocat", rule.Match.Claims["/actor"])
	require.Equal(t, []string{"instance:read", "instance:write"}, rule.Authorization.Permissions)
}

// The server compiles its configuration once to validate it and again to use
// it, so normalizing inline keys must leave the configured entries alone.
func TestCompileLeavesInlineKeysAsConfigured(t *testing.T) {
	f := newAssertionFixture(t)
	var set map[string][]map[string]any
	require.NoError(t, json.Unmarshal(f.keys, &set))
	signing := set["keys"][0]
	encryption := maps.Clone(signing)
	encryption["use"], encryption["alg"] = "enc", "RSA-OAEP"
	cfg := Config{
		Issuers: []IssuerSpec{{Name: "idp", IssuerURL: "https://idp.example.test", JWKS: KeySource{Type: "inline", Keys: []map[string]any{encryption, signing}}}},
		FederationRules: []RuleSpec{{
			Name: "ci", Issuer: "idp", ExpectedAudience: "https://auth.example.test",
			Match:         Match{Type: "custom_oidc", Subject: "build"},
			Authorization: RuleAuthorization{Permissions: []string{"image:read"}},
		}},
	}
	var revisions []string
	for range 2 {
		registry, err := Compile(cfg)
		require.NoError(t, err)
		rule, _ := registry.Rule("ci")
		require.Len(t, rule.IssuerSpec.JWKS.Keys, 1, "only the signing key is kept")
		require.Equal(t, "sig", rule.IssuerSpec.JWKS.Keys[0]["use"])
		revisions = append(revisions, rule.Revision)
	}
	require.Equal(t, revisions[0], revisions[1])
	require.Len(t, cfg.Issuers[0].JWKS.Keys, 2)
	require.Equal(t, "enc", cfg.Issuers[0].JWKS.Keys[0]["use"])
	require.Equal(t, "sig", cfg.Issuers[0].JWKS.Keys[1]["use"])
}

func TestJSONPointerStringClaims(t *testing.T) {
	claims := map[string]any{"literal/key": map[string]any{"a~b": "proj_a"}, "kubernetes.io": map[string]any{"namespace": "ns"}, "values": []any{"first"}, "number": 12}
	for pointer, expected := range map[string]string{"/literal~1key/a~0b": "proj_a", "/kubernetes.io/namespace": "ns", "/values/0": "first"} {
		value, ok := ClaimString(claims, pointer)
		require.True(t, ok)
		require.Equal(t, expected, value)
	}
	for _, pointer := range []string{"", "kubernetes.io.namespace", "/bad~2escape", "/values/00", "/values/-", "/values/1", "/missing", "/number"} {
		_, ok := ClaimString(claims, pointer)
		require.False(t, ok, pointer)
	}
}

func TestExpiryClipsToTheAssertion(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, test := range []struct{ assertion, expected int64 }{
		{2000, 1300},
		{1010, 1010},
	} {
		expiry, err := AccessExpiry(now, time.Unix(test.assertion, 0), DefaultTokenTTL)
		require.NoError(t, err)
		require.Equal(t, test.expected, expiry.Unix())
	}
	_, err := AccessExpiry(now, now, DefaultTokenTTL)
	require.ErrorIs(t, err, ErrAssertion)
	_, err = AccessExpiry(now, now.Add(time.Hour), MaxTokenTTL+1)
	require.ErrorIs(t, err, ErrInvalid)
}

// Google sends email_verified as a boolean and project numbers as numbers. A
// condition written as text has to match them, or the rule can never succeed.
func TestClaimConditionsMatchBooleansAndNumbersByTheirText(t *testing.T) {
	claims := map[string]any{
		"sub": "107517467455664443765", "email_verified": true, "disabled": false,
		"google": map[string]any{"compute_engine": map[string]any{"project_number": json.Number("739419398126")}},
		"ratio":  1.5, "groups": []any{"a"}, "nested": map[string]any{"k": "v"},
	}
	for pointer, expected := range map[string]string{
		"/email_verified": "true", "/disabled": "false",
		"/google/compute_engine/project_number": "739419398126", "/ratio": "1.5",
	} {
		value, ok := ClaimString(claims, pointer)
		require.True(t, ok, pointer)
		require.Equal(t, expected, value, pointer)
	}
	for _, pointer := range []string{"/groups", "/nested"} {
		_, ok := ClaimString(claims, pointer)
		require.False(t, ok, "%s is not a scalar", pointer)
	}
	match := Match{Type: "google_cloud", Subject: "107517467455664443765", Claims: map[string]string{"/email_verified": "true"}}
	require.True(t, match.Matches(claims))
	match.Claims["/email_verified"] = "false"
	require.False(t, match.Matches(claims))
}
