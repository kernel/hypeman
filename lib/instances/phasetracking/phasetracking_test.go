package phasetracking

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRecord_FirstTransitionAccruesNothing(t *testing.T) {
	var tr Tracker
	now := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)

	tr.Record(PhaseRunning, now)

	if tr.Current != PhaseRunning {
		t.Errorf("Current = %q, want %q", tr.Current, PhaseRunning)
	}
	if !tr.Since.Equal(now) {
		t.Errorf("Since = %v, want %v", tr.Since, now)
	}
	if got := tr.Cumulative[PhaseRunning]; got != 0 {
		t.Errorf("Cumulative[running] = %d, want 0 on first transition", got)
	}
}

func TestRecord_AccruesPriorPhaseOnTransition(t *testing.T) {
	var tr Tracker
	t0 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)

	tr.Record(PhaseRunning, t0)
	tr.Record(PhaseStandby, t0.Add(30*time.Second))
	tr.Record(PhaseRunning, t0.Add(30*time.Second+5*time.Minute))
	tr.Record(PhaseStopped, t0.Add(30*time.Second+5*time.Minute+10*time.Second))

	if got, want := tr.Cumulative[PhaseRunning], int64(40_000); got != want {
		t.Errorf("Cumulative[running] = %d, want %d", got, want)
	}
	if got, want := tr.Cumulative[PhaseStandby], int64(300_000); got != want {
		t.Errorf("Cumulative[standby] = %d, want %d", got, want)
	}
	if tr.Current != PhaseStopped {
		t.Errorf("Current = %q, want %q", tr.Current, PhaseStopped)
	}
}

func TestRecord_ZeroOrNegativeElapsedIsNoOp(t *testing.T) {
	var tr Tracker
	t0 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)

	tr.Record(PhaseRunning, t0)
	// Same instant — no time elapsed.
	tr.Record(PhaseStandby, t0)
	// Backward clock — also no accrual.
	tr.Record(PhaseRunning, t0.Add(-5*time.Second))

	if got := tr.Cumulative[PhaseRunning]; got != 0 {
		t.Errorf("Cumulative[running] = %d, want 0 (zero/negative elapsed)", got)
	}
	if got := tr.Cumulative[PhaseStandby]; got != 0 {
		t.Errorf("Cumulative[standby] = %d, want 0 (zero/negative elapsed)", got)
	}
}

func TestSnapshot_IncludesLiveTime(t *testing.T) {
	var tr Tracker
	t0 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)

	tr.Record(PhaseRunning, t0)
	tr.Record(PhaseStandby, t0.Add(60*time.Second))

	// Now we're 10s into standby. Cumulative shouldn't yet include this.
	live := tr.Snapshot(t0.Add(70 * time.Second))

	if got, want := tr.Cumulative[PhaseRunning], int64(60_000); got != want {
		t.Errorf("Cumulative[running] = %d, want %d", got, want)
	}
	if _, present := tr.Cumulative[PhaseStandby]; present {
		t.Errorf("Cumulative should not yet contain standby")
	}
	if got, want := live[PhaseRunning], int64(60_000); got != want {
		t.Errorf("Snapshot[running] = %d, want %d", got, want)
	}
	if got, want := live[PhaseStandby], int64(10_000); got != want {
		t.Errorf("Snapshot[standby] = %d, want %d", got, want)
	}
}

func TestSnapshot_DoesNotMutate(t *testing.T) {
	var tr Tracker
	t0 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	tr.Record(PhaseRunning, t0)

	_ = tr.Snapshot(t0.Add(5 * time.Minute))

	if got := tr.Cumulative[PhaseRunning]; got != 0 {
		t.Errorf("Cumulative[running] mutated by Snapshot: got %d, want 0", got)
	}
}

func TestReset_ClearsAll(t *testing.T) {
	var tr Tracker
	t0 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	tr.Record(PhaseRunning, t0)
	tr.Record(PhaseStandby, t0.Add(time.Minute))

	tr.Reset()

	if tr.Current != "" || !tr.Since.IsZero() || tr.Cumulative != nil {
		t.Errorf("Reset did not clear all fields: %+v", tr)
	}
}

func TestJSONRoundTrip_PreservesTracker(t *testing.T) {
	t0 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	tr := Tracker{
		Current: PhaseStandby,
		Since:   t0,
		Cumulative: map[Phase]int64{
			PhaseRunning: 12_345,
			PhaseStandby: 67_890,
		},
	}

	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Tracker
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Current != tr.Current || !got.Since.Equal(tr.Since) {
		t.Errorf("roundtrip current/since mismatch: %+v", got)
	}
	if got.Cumulative[PhaseRunning] != 12_345 || got.Cumulative[PhaseStandby] != 67_890 {
		t.Errorf("roundtrip cumulative mismatch: %+v", got.Cumulative)
	}
}

func TestJSONRoundTrip_ZeroValueOmitted(t *testing.T) {
	// A fresh metadata file written before this feature shipped will not
	// contain the `phases` field. Unmarshalling must succeed and produce a
	// zero-value Tracker so the first Record call works as a fresh start.
	var tr Tracker
	if err := json.Unmarshal([]byte(`{}`), &tr); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	tr.Record(PhaseRunning, time.Now())
	if tr.Current != PhaseRunning {
		t.Errorf("Current after fresh Record = %q, want running", tr.Current)
	}
}

// Regression: a session that spends 60s running then 300s in standby then
// 30s running again must report 90s running and 300s standby for billing.
func TestRecord_BillingScenario(t *testing.T) {
	var tr Tracker
	t0 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)

	tr.Record(PhaseRunning, t0)
	tr.Record(PhaseStandby, t0.Add(60*time.Second))
	tr.Record(PhaseRunning, t0.Add(60*time.Second+300*time.Second))
	tr.Record(PhaseStopped, t0.Add(60*time.Second+300*time.Second+30*time.Second))

	billableRunning := tr.Cumulative[PhaseRunning]
	standby := tr.Cumulative[PhaseStandby]

	if billableRunning != 90_000 {
		t.Errorf("billable running ms = %d, want 90000", billableRunning)
	}
	if standby != 300_000 {
		t.Errorf("standby ms = %d, want 300000", standby)
	}
}
