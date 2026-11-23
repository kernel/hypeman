package oidcfederation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// SubjectPrefix marks the `sub` of a token issued for a federation rule.
	SubjectPrefix = "federation:"
	// maxSpentAssertions bounds the replay cache. At the limit new single-use
	// assertions are refused rather than accepted without a replay record.
	maxSpentAssertions = 100_000
	spentSweepPeriod   = time.Minute
	// tokenRequestReadTimeout bounds how long a caller may take to send the form.
	tokenRequestReadTimeout = 10 * time.Second
)

// Token is a short-lived API token issued for a federation rule.
type Token struct {
	AccessToken string
	ExpiresIn   int
	Permissions []string
	Principal   string
	Rule        string
}

// Exchanger trades verified external assertions for API tokens signed with the
// server's own key. It holds the only record of spent assertions, in memory.
type Exchanger struct {
	registry    *Registry
	verifier    *Verifier
	secret      []byte
	startedAt   time.Time
	readTimeout time.Duration
	now         func() time.Time
	exchanges   metric.Int64Counter

	mu         sync.Mutex
	spent      map[string]time.Time
	sweepAfter time.Time
}

// NewExchanger issues tokens signed with signingSecret for the rules in registry.
func NewExchanger(registry *Registry, signingSecret string, meter metric.Meter) (*Exchanger, error) {
	if signingSecret == "" {
		return nil, errors.New("JWT_SECRET must be set")
	}
	exchanges, err := meter.Int64Counter("hypeman_oidc_exchanges_total",
		metric.WithDescription("OIDC assertion exchanges by federation rule, issuer and result"))
	if err != nil {
		return nil, err
	}
	return &Exchanger{
		registry:    registry,
		verifier:    NewVerifier(),
		secret:      []byte(signingSecret),
		startedAt:   time.Now(),
		readTimeout: tokenRequestReadTimeout,
		now:         time.Now,
		exchanges:   exchanges,
		spent:       make(map[string]time.Time),
	}, nil
}

// Enabled reports whether any federation rule is configured.
func (e *Exchanger) Enabled() bool {
	return e.registry.Enabled()
}

// Current reports whether a token issued for a rule still matches the running
// configuration: the rule exists, trusts the same issuer, and is unchanged.
func (e *Exchanger) Current(issuer, rule, revision string) bool {
	configured, ok := e.registry.Rule(rule)
	return ok && configured.Issuer == issuer && configured.Revision == revision
}

// Exchange verifies the assertion against the one rule the caller names. The
// rule name is an untrusted selector: it picks which rule to check, never what
// is granted. Every refusal is ErrAssertion; ErrUnavailable means a token
// cannot be issued right now, whatever the assertion.
func (e *Exchanger) Exchange(ctx context.Context, ruleName, raw string) (Token, error) {
	token, outcome := e.exchange(ctx, ruleName, raw)
	e.exchanges.Add(ctx, 1, metric.WithAttributes(
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
		attrs = append(attrs, "token_jti", outcome.tokenID)
	}
	log := logger.FromContext(ctx)
	level := slog.LevelInfo
	if outcome.result == "unavailable" {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "oidc assertion exchange", attrs...)
	return token, outcome.err
}

// exchangeOutcome is what the metric and the log line record. The rule and
// issuer are left empty unless the caller named a configured rule, so request
// input never becomes a metric label.
type exchangeOutcome struct {
	rule, issuer     string
	result, reason   string
	subject, tokenID string
	err              error
}

func (e *Exchanger) exchange(ctx context.Context, ruleName, raw string) (Token, exchangeOutcome) {
	refuse := func(o exchangeOutcome, reason string) (Token, exchangeOutcome) {
		o.result, o.reason, o.err = "rejected", reason, ErrAssertion
		return Token{}, o
	}
	rule, ok := e.registry.Rule(ruleName)
	if !ok {
		return refuse(exchangeOutcome{}, "unknown rule")
	}
	outcome := exchangeOutcome{rule: rule.Name, issuer: rule.Issuer}
	assertion, err := e.verifier.Verify(ctx, rule.IssuerSpec, rule.ExpectedAudience, raw)
	if err != nil {
		if errors.Is(err, ErrAssertion) {
			return refuse(outcome, "assertion not valid for this issuer and audience")
		}
		outcome.result, outcome.reason, outcome.err = "unavailable", err.Error(), ErrUnavailable
		return Token{}, outcome
	}
	outcome.subject = assertion.Subject
	if !rule.Match.Matches(assertion.Claims) {
		return refuse(outcome, "claims do not match the rule")
	}
	now := e.now()
	// The record of spent assertions does not survive a restart. Anything the
	// previous process could have spent was accepted before this one started,
	// so its iat is earlier than the start time plus the clock leeway the
	// verifier allows. Refusing everything below that floor keeps single use
	// across restarts without trusting the issuer's clock to match this host's.
	if floor := e.startedAt.Add(clockLeeway); assertion.SingleUse && assertion.IssuedAt.Before(floor) {
		// A fresh assertion from an issuer whose clock runs behind by up to
		// the leeway stays below the floor that much longer, and iat is in
		// whole seconds. Until then the refusal is worth retrying.
		if now.Before(floor.Add(clockLeeway + time.Second)) {
			outcome.result, outcome.reason, outcome.err = "unavailable", "server started recently; this single-use assertion is not accepted yet", ErrUnavailable
			return Token{}, outcome
		}
		return refuse(outcome, "assertion was issued before this server could vouch for it being unused")
	}
	expiry, err := AccessExpiry(now, assertion.ExpiresAt, *rule.Authorization.MaxTokenTTLSeconds)
	if err != nil {
		return refuse(outcome, "assertion expires too soon")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		outcome.result, outcome.reason, outcome.err = "unavailable", "token ID generation failed", ErrUnavailable
		return Token{}, outcome
	}
	tokenID := hex.EncodeToString(id[:])
	principal := SubjectPrefix + rule.Name
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":         principal,
		"iat":         now.Unix(),
		"exp":         expiry.Unix(),
		"jti":         tokenID,
		"permissions": rule.Authorization.Permissions,
		"fed":         map[string]string{"issuer": rule.Issuer, "rule": rule.Name, "rev": rule.Revision},
	}).SignedString(e.secret)
	if err != nil {
		outcome.result, outcome.reason, outcome.err = "unavailable", "token signing failed", ErrUnavailable
		return Token{}, outcome
	}
	// Consume the assertion last, so a failure above leaves it usable.
	if assertion.SingleUse {
		switch e.consume(rule.Issuer, assertion.JTI, assertion.ExpiresAt, now) {
		case spentAlready:
			return refuse(outcome, "assertion was already used")
		case spentFull:
			outcome.result, outcome.reason, outcome.err = "unavailable", "replay cache is full", ErrUnavailable
			return Token{}, outcome
		}
	}
	outcome.result, outcome.reason, outcome.tokenID = "issued", "issued", tokenID
	return Token{
		AccessToken: signed,
		// The lifetime the token itself states: exp minus iat, both whole seconds.
		ExpiresIn:   int(expiry.Unix() - now.Unix()),
		Permissions: rule.Authorization.Permissions,
		Principal:   principal,
		Rule:        rule.Name,
	}, outcome
}

type spentResult int

const (
	spentRecorded spentResult = iota
	spentAlready
	spentFull
)

// consume records a single-use assertion. Uniqueness is per issuer, so one
// assertion cannot be spent once for each rule that would accept it.
func (e *Exchanger) consume(issuer, jti string, expires, now time.Time) spentResult {
	key := issuer + "\x00" + jti
	e.mu.Lock()
	defer e.mu.Unlock()
	if until, exists := e.spent[key]; exists && now.Before(until) {
		return spentAlready
	}
	if len(e.spent) >= maxSpentAssertions || !now.Before(e.sweepAfter) {
		for spentKey, until := range e.spent {
			if !now.Before(until) {
				delete(e.spent, spentKey)
			}
		}
		e.sweepAfter = now.Add(spentSweepPeriod)
	}
	if len(e.spent) >= maxSpentAssertions {
		return spentFull
	}
	// The verifier accepts an assertion until its expiry plus the clock leeway.
	e.spent[key] = expires.Add(clockLeeway)
	return spentRecorded
}
