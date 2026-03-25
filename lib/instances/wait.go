package instances

import (
	"context"
	"errors"
	"time"
)

const (
	WaitForStateDefaultTimeout = 60 * time.Second
	WaitForStateMaxTimeout     = 5 * time.Minute
	WaitForStatePollInterval   = 1 * time.Second
)

// WaitForStateResult is the outcome of a WaitForState call.
type WaitForStateResult struct {
	State      State
	StateError *string
	TimedOut   bool
}

// WaitForState polls until the instance reaches targetState, a terminal/error
// state is detected, the timeout expires, or the context is cancelled.
// The caller must supply the current instance snapshot so the function can
// short-circuit when the instance is already in the target state.
func WaitForState(ctx context.Context, mgr Manager, inst *Instance, targetState State, timeout time.Duration) (*WaitForStateResult, error) {
	// Already in target state — return immediately.
	if inst.State == targetState {
		return &WaitForStateResult{
			State:      inst.State,
			StateError: inst.StateError,
		}, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(WaitForStatePollInterval)
	defer ticker.Stop()

	id := inst.Id
	latest := inst

	for {
		select {
		case <-ctx.Done():
			return &WaitForStateResult{
				State:    latest.State,
				TimedOut: true,
			}, nil

		case <-timer.C:
			if got, err := mgr.GetInstance(ctx, id); err == nil {
				latest = got
			}
			return &WaitForStateResult{
				State:      latest.State,
				StateError: latest.StateError,
				TimedOut:   true,
			}, nil

		case <-ticker.C:
			got, err := mgr.GetInstance(ctx, id)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, ErrNotFound
				}
				continue // transient error — retry on next tick
			}
			latest = got

			if latest.State == targetState {
				return &WaitForStateResult{
					State:      latest.State,
					StateError: latest.StateError,
				}, nil
			}

			// Terminal state — won't change further.
			if latest.State == StateStopped && targetState != StateStopped {
				return &WaitForStateResult{
					State:      latest.State,
					StateError: latest.StateError,
				}, nil
			}

			// Unknown state — something is wrong.
			if latest.State == StateUnknown {
				return &WaitForStateResult{
					State:      latest.State,
					StateError: latest.StateError,
				}, nil
			}
		}
	}
}
