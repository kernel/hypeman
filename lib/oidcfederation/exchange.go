package oidcfederation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	// spentLogSlack is how many expired lines the file may hold beyond twice
	// the live ones before it is rewritten.
	spentLogSlack = 1024
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
// server's own key. It keeps the record of spent single-use assertions, in
// memory and in a file that survives restarts.
type Exchanger struct {
	registry    *Registry
	verifier    *Verifier
	secret      []byte
	readTimeout time.Duration
	now         func() time.Time
	exchanges   metric.Int64Counter

	mu sync.Mutex
	// spent holds exactly the unexpired entries recorded in spentLog.
	spent      map[string]time.Time
	spentLog   *spentLog
	sweepAfter time.Time
	// spentComplete is false when the record on disk could not vouch for
	// every assertion an earlier process spent. This start then raised the
	// record's floor.
	spentComplete bool
}

// NewExchanger issues tokens signed with signingSecret for the rules in
// registry. Spent single-use assertions are recorded in the file at spentPath;
// without any rule the file is not touched.
func NewExchanger(registry *Registry, signingSecret, spentPath string, meter metric.Meter) (*Exchanger, error) {
	return newExchanger(registry, signingSecret, spentPath, meter, time.Now())
}

func newExchanger(registry *Registry, signingSecret, spentPath string, meter metric.Meter, startedAt time.Time) (*Exchanger, error) {
	if signingSecret == "" {
		return nil, errors.New("JWT_SECRET must be set")
	}
	exchanges, err := meter.Int64Counter("hypeman_oidc_exchanges_total",
		metric.WithDescription("OIDC assertion exchanges by federation rule, issuer and result"))
	if err != nil {
		return nil, err
	}
	e := &Exchanger{
		registry:    registry,
		verifier:    NewVerifier(),
		secret:      []byte(signingSecret),
		readTimeout: tokenRequestReadTimeout,
		now:         time.Now,
		exchanges:   exchanges,
		spent:       make(map[string]time.Time),
	}
	if registry.Enabled() {
		// Anything an earlier process could have spent was accepted before
		// this one started, so its iat is earlier than the start time plus
		// the clock leeway the verifier allows. Without a complete record,
		// refusing everything below that floor keeps single use without
		// trusting the issuer's clock to match this host's.
		e.spentLog, e.spent, e.spentComplete, err = openSpentLog(spentPath, startedAt, startedAt.Add(clockLeeway))
		if err != nil {
			return nil, fmt.Errorf("open the record of spent identity tokens: %w", err)
		}
	}
	return e, nil
}

// SpentRecordComplete reports whether the record of spent single-use
// assertions was read in full at start. When it was not, this start raised
// the record's floor.
func (e *Exchanger) SpentRecordComplete() bool {
	return e.spentComplete
}

// SpentRecordFloor returns the issue time below which single-use assertions
// are refused because the record cannot say whether they were spent. It is
// zero without a federation rule.
func (e *Exchanger) SpentRecordFloor() time.Time {
	if e.spentLog == nil {
		return time.Time{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spentLog.floor
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
	ttl := *rule.Authorization.MaxTokenTTLSeconds
	if _, err := AccessExpiry(now, assertion.ExpiresAt, ttl); err != nil {
		return refuse(outcome, "assertion expires too soon")
	}
	// Everything above leaves a refused assertion usable. Once consumed it is spent.
	if assertion.SingleUse {
		switch result, err := e.consume(rule.IssuerSpec.IssuerURL, assertion.JTI, assertion.IssuedAt, assertion.ExpiresAt, now); result {
		case spentAlready:
			return refuse(outcome, "assertion was already used")
		case spentUnknown:
			return refuse(outcome, "assertion was issued before the record of spent assertions can vouch for it being unused")
		case spentUnknownForNow:
			outcome.result, outcome.reason, outcome.err = "unavailable", "the record of spent assertions cannot vouch for this assertion being unused; a later one will be accepted", ErrUnavailable
			return Token{}, outcome
		case spentFull:
			outcome.result, outcome.reason, outcome.err = "unavailable", "the record of spent assertions is full", ErrUnavailable
			return Token{}, outcome
		case spentUnrecorded:
			outcome.result, outcome.reason, outcome.err = "unavailable", "the spent assertion could not be recorded: "+err.Error(), ErrUnavailable
			return Token{}, outcome
		}
	}
	// Recording the assertion can take a while. The token's lifetime starts
	// now, so that a token is never returned already expired.
	now = e.now()
	expiry, err := AccessExpiry(now, assertion.ExpiresAt, ttl)
	if err != nil {
		return refuse(outcome, "assertion expired while it was being recorded")
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
	spentUnrecorded
	// spentUnknown: the record cannot say whether the assertion was spent.
	spentUnknown
	// spentUnknownForNow: the same, while an assertion obtained now may hit
	// the same limit, so retrying with a fresh one is worthwhile.
	spentUnknownForNow
)

// consume records a single-use assertion, in memory and on disk, before its
// token is returned. An assertion that could not be written to disk is not
// recorded in memory either, and no token is issued for it.
func (e *Exchanger) consume(issuerURL, jti string, issuedAt, expires, now time.Time) (spentResult, error) {
	key := spentKey(issuerURL, jti)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.spent) >= maxSpentAssertions || !now.Before(e.sweepAfter) {
		for spentKey, until := range e.spent {
			if !now.Before(until) {
				delete(e.spent, spentKey)
				e.spentLog.forget(until)
			}
		}
		e.sweepAfter = now.Add(spentSweepPeriod)
		// Expired lines stay in the file until it is rewritten. Do that once
		// they outnumber the live ones. A failure either leaves the old file
		// in use or marks the log dirty, which the append below must clear.
		if e.spentLog.lines > 2*len(e.spent)+spentLogSlack {
			_ = e.spentLog.rewrite(e.spent)
		}
	}
	// Below the record's floor nothing says whether the assertion was spent.
	if floor := e.spentLog.floor; issuedAt.Before(floor) {
		// A fresh assertion from an issuer whose clock runs behind by up to
		// the leeway stays below the floor that much longer, and iat is in
		// whole seconds.
		if now.Before(floor.Add(clockLeeway + time.Second)) {
			return spentUnknownForNow, nil
		}
		return spentUnknown, nil
	}
	// An entry that was dropped as expired had an expiry at or before this
	// mark, so an assertion expiring that early may be one of them. With a
	// correct clock no assertion still within its lifetime does; after a
	// clock that ran ahead, one obtained later expires past the mark.
	if !expires.After(e.spentLog.dropped) {
		return spentUnknownForNow, nil
	}
	if until, exists := e.spent[key]; exists && now.Before(until) {
		return spentAlready, nil
	}
	if len(e.spent) >= maxSpentAssertions {
		return spentFull, nil
	}
	// The verifier accepts an assertion until its expiry plus the clock leeway.
	until := expires.Add(clockLeeway)
	if err := e.spentLog.append(key, until, e.spent); err != nil {
		return spentUnrecorded, err
	}
	e.spent[key] = until
	return spentRecorded, nil
}
