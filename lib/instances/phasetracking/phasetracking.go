// Package phasetracking accumulates cumulative time-in-phase for instance
// lifecycle phases (running, standby, paused, etc.). The tracker is embedded
// in instance metadata and updated at every state transition. Consumers use
// the resulting durations for billing, observability, and analytics.
//
// Only the transition orchestration sites in lib/instances should call Record.
// The tracker intentionally does not subscribe to the lifecycle event bus —
// that bus is best-effort and lossy, which is unsuitable for billing.
package phasetracking

import "time"

// Phase is the canonical lifecycle phase name. Values mirror instances.State
// lowercased so they remain stable in the API surface even if the internal
// State enum is renamed.
type Phase string

const (
	PhaseStopped      Phase = "stopped"
	PhaseCreated      Phase = "created"
	PhaseInitializing Phase = "initializing"
	PhaseRunning      Phase = "running"
	PhasePaused       Phase = "paused"
	PhaseShutdown     Phase = "shutdown"
	PhaseStandby      Phase = "standby"
)

// Tracker accumulates cumulative wall-clock time spent in each phase.
//
// Invariants:
//   - Cumulative[phase] is the total ms spent in `phase` across all prior
//     completed visits to that phase.
//   - Time spent in the *current* phase (since `Since`) is NOT yet rolled into
//     Cumulative — callers that want "live" totals should use Snapshot.
//   - Current and Since must be updated atomically with Cumulative; that's
//     the contract of Record. Direct mutation is not supported.
//
// The zero value is valid: it represents an instance that has not entered
// any phase yet. The first Record call sets Current and Since without
// accruing time (there is no prior phase to accrue from).
type Tracker struct {
	Current    Phase           `json:"current,omitempty"`
	Since      time.Time       `json:"since,omitempty"`
	Cumulative map[Phase]int64 `json:"cumulative,omitempty"`
}

// Record transitions into newPhase as of `now`, first accruing time-in-current
// into Cumulative. Safe to call on a zero-value Tracker (first transition has
// no prior phase, so no accrual happens).
//
// `now` is a parameter rather than time.Now() so tests can pin time and so
// callers can use the same `now` value they're persisting elsewhere on the
// metadata (e.g. StartedAt) without drift.
func (t *Tracker) Record(newPhase Phase, now time.Time) {
	if t.Cumulative == nil {
		t.Cumulative = make(map[Phase]int64)
	}
	if t.Current != "" && !t.Since.IsZero() {
		elapsed := now.Sub(t.Since).Milliseconds()
		if elapsed > 0 {
			t.Cumulative[t.Current] += elapsed
		}
	}
	t.Current = newPhase
	t.Since = now
}

// Snapshot returns a copy of Cumulative with the in-flight time-in-current
// rolled in, without mutating the tracker. Use this when reporting "running
// time so far" — typically in the API response path.
func (t Tracker) Snapshot(now time.Time) map[Phase]int64 {
	out := make(map[Phase]int64, len(t.Cumulative)+1)
	for k, v := range t.Cumulative {
		out[k] = v
	}
	if t.Current != "" && !t.Since.IsZero() {
		elapsed := now.Sub(t.Since).Milliseconds()
		if elapsed > 0 {
			out[t.Current] += elapsed
		}
	}
	return out
}

// Reset clears all accumulated state. Used when forking — the fork is a new
// instance and must not inherit the source's phase history.
func (t *Tracker) Reset() {
	t.Current = ""
	t.Since = time.Time{}
	t.Cumulative = nil
}
