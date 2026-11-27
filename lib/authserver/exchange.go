// Package authserver is the OAuth authorization server for Hypeman: it
// exchanges a workload's OIDC identity token for a short-lived access token
// that Hypeman servers verify with its public keys.
package authserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/oidcfederation"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// SubjectPrefix starts the `sub` of every token issued for a federation rule.
const SubjectPrefix = "federation:"

var (
	// ErrGrant is returned for every identity token that is not accepted,
	// whatever the reason.
	ErrGrant = errors.New("identity token not accepted")
	// ErrTarget means the caller was identified but may not have a token for
	// the audience it asked for.
	ErrTarget = errors.New("audience not allowed")
	// ErrUnavailable means no token can be issued right now, whatever the
	// identity token.
	ErrUnavailable = errors.New("token issuing unavailable")
)

// Options configure a Server.
type Options struct {
	// Issuer is the server's issuer URL, the `iss` of its tokens.
	Issuer string
	Key    *SigningKey
	// PublishedKeys are public keys published beside the signing key: the
	// previous key while its tokens are in use, or the next one before it signs.
	PublishedKeys []*ecdsa.PublicKey
	Rules         *oidcfederation.Registry
	// Spent records single-use identity tokens. It may be nil only if no
	// rule's issuer makes its identity tokens single-use.
	Spent SpentStore
	Meter metric.Meter
	Log   *slog.Logger
}

// Server issues access tokens for the configured federation rules.
type Server struct {
	issuer    string
	key       *SigningKey
	keySet    []byte
	metadata  []byte
	rules     *oidcfederation.Registry
	verifier  *oidcfederation.Verifier
	spent     SpentStore
	exchanges metric.Int64Counter
	log       *slog.Logger
	now       func() time.Time
}

// New validates the options.
func New(options Options) (*Server, error) {
	issuer, err := url.Parse(options.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || issuer.User != nil || strings.Trim(issuer.EscapedPath(), "/") != "" || strings.ContainsAny(options.Issuer, "?#") {
		return nil, fmt.Errorf("issuer must be an https URL without a path, query or fragment")
	}
	if options.Key == nil {
		return nil, fmt.Errorf("a signing key is required")
	}
	if options.Spent == nil && options.Rules.SingleUse() {
		return nil, fmt.Errorf("a replay store is required: at least one configured issuer's identity tokens are single-use")
	}
	exchanges, err := options.Meter.Int64Counter("hypeman_auth_exchanges_total",
		metric.WithDescription("Identity token exchanges by federation rule, issuer and result"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		issuer:    strings.TrimRight(options.Issuer, "/"),
		key:       options.Key,
		rules:     options.Rules,
		verifier:  oidcfederation.NewVerifier(),
		spent:     options.Spent,
		exchanges: exchanges,
		log:       options.Log,
		now:       time.Now,
	}
	if s.keySet, err = keySet(options.Key, options.PublishedKeys); err != nil {
		return nil, err
	}
	if s.metadata, err = metadata(s.issuer); err != nil {
		return nil, err
	}
	return s, nil
}

// Token is an issued access token.
type Token struct {
	AccessToken string
	ExpiresIn   int
	Scope       string
}

// Exchange verifies the identity token against the one rule the caller names
// and issues an access token for audience, or for the rule's only audience
// when audience is empty. The rule name is an untrusted selector: it picks
// which rule to check, never what is granted.
func (s *Server) Exchange(ctx context.Context, ruleName, assertion, audience string) (Token, error) {
	token, outcome := s.exchange(ctx, ruleName, assertion, audience)
	s.exchanges.Add(ctx, 1, metric.WithAttributes(
		attribute.String("issuer", outcome.issuer),
		attribute.String("rule", outcome.rule),
		attribute.String("result", outcome.result),
	))
	attrs := []any{"rule", outcome.rule, "issuer", outcome.issuer, "result", outcome.result, "reason", outcome.reason}
	if outcome.subject != "" {
		digest := sha256.Sum256([]byte(outcome.subject))
		attrs = append(attrs, "subject_sha256", hex.EncodeToString(digest[:]))
	}
	if outcome.tokenID != "" {
		attrs = append(attrs, "token_jti", outcome.tokenID, "audience", outcome.audience)
	}
	level := slog.LevelInfo
	if outcome.result == "unavailable" {
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, "identity token exchange", attrs...)
	return token, outcome.err
}

// exchangeOutcome is what the metric and the log line record. The rule and
// issuer are left empty unless the caller named a configured rule, so request
// input never becomes a metric label.
type exchangeOutcome struct {
	rule, issuer               string
	result, reason             string
	subject, tokenID, audience string
	err                        error
}

func (s *Server) exchange(ctx context.Context, ruleName, raw, audience string) (Token, exchangeOutcome) {
	fail := func(o exchangeOutcome, result, reason string, err error) (Token, exchangeOutcome) {
		o.result, o.reason, o.err = result, reason, err
		return Token{}, o
	}
	rule, ok := s.rules.Rule(ruleName)
	if !ok {
		return fail(exchangeOutcome{}, "rejected", "unknown rule", ErrGrant)
	}
	outcome := exchangeOutcome{rule: rule.Name, issuer: rule.Issuer}
	assertion, err := s.verifier.Verify(ctx, rule.IssuerSpec, rule.ExpectedAudience, raw)
	if errors.Is(err, oidcfederation.ErrAssertion) {
		return fail(outcome, "rejected", "identity token not valid for this issuer and audience", ErrGrant)
	}
	if err != nil {
		return fail(outcome, "unavailable", err.Error(), ErrUnavailable)
	}
	outcome.subject = assertion.Subject
	if !rule.Match.Matches(assertion.Claims) {
		return fail(outcome, "rejected", "claims do not match the rule", ErrGrant)
	}
	subject := SubjectPrefix + rule.Name
	if pointer := rule.Authorization.SubjectClaim; pointer != "" {
		caller, found := oidcfederation.ClaimString(assertion.Claims, pointer)
		if !found || caller == "" || len(caller) > 256 || strings.ContainsFunc(caller, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return fail(outcome, "rejected", "the subject claim is missing or not usable", ErrGrant)
		}
		subject += ":" + caller
	}
	// The caller is now known to be the workload the rule names, so it may be
	// told that the audience it asked for is not one of the rule's.
	audiences := rule.Authorization.Audiences
	if audience == "" && len(audiences) == 1 {
		audience = audiences[0]
	}
	if !slices.Contains(audiences, audience) {
		return fail(outcome, "rejected", "audience not allowed by the rule", ErrTarget)
	}
	ttl := *rule.Authorization.MaxTokenTTLSeconds
	if _, err := oidcfederation.AccessExpiry(s.now(), assertion.ExpiresAt, ttl); err != nil {
		return fail(outcome, "rejected", "identity token expires too soon", ErrGrant)
	}
	// Everything above leaves a refused identity token usable. Once recorded it is spent.
	if assertion.SingleUse {
		digest := sha256.Sum256([]byte(rule.IssuerSpec.IssuerURL + "\n" + assertion.JTI))
		first, err := s.spent.Spend(ctx, hex.EncodeToString(digest[:]), assertion.ExpiresAt)
		if err != nil {
			return fail(outcome, "unavailable", "the spent identity token could not be recorded: "+err.Error(), ErrUnavailable)
		}
		if !first {
			return fail(outcome, "rejected", "identity token was already used", ErrGrant)
		}
	}
	// Recording can take a while. The token's lifetime starts now, so that a
	// token is never returned already expired.
	now := s.now()
	expiry, err := oidcfederation.AccessExpiry(now, assertion.ExpiresAt, ttl)
	if err != nil {
		return fail(outcome, "rejected", "identity token expired while it was being recorded", ErrGrant)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fail(outcome, "unavailable", "token ID generation failed", ErrUnavailable)
	}
	tokenID := hex.EncodeToString(id[:])
	scope := strings.Join(rule.Authorization.Permissions, " ")
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":       s.issuer,
		"sub":       subject,
		"aud":       []string{audience},
		"client_id": rule.Name,
		"scope":     scope,
		"iat":       now.Unix(),
		"exp":       expiry.Unix(),
		"jti":       tokenID,
	})
	token.Header["typ"] = "at+jwt"
	token.Header["kid"] = s.key.ID
	signed, err := token.SignedString(s.key.private)
	if err != nil {
		return fail(outcome, "unavailable", "token signing failed", ErrUnavailable)
	}
	outcome.result, outcome.reason, outcome.tokenID, outcome.audience = "issued", "issued", tokenID, audience
	// The lifetime the token itself states: exp minus iat, both whole seconds.
	return Token{AccessToken: signed, ExpiresIn: int(expiry.Unix() - now.Unix()), Scope: scope}, outcome
}
