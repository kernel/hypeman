package instances

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

const (
	WaitForStateDefaultTimeout = 60 * time.Second
	WaitForStateMaxTimeout     = 5 * time.Minute
	WaitForStatePollInterval   = 5 * time.Second
)

// WaitForStateResult is the outcome of a WaitForState call.
type WaitForStateResult struct {
	State      State
	StateError *string
	TimedOut   bool
}

// WaitForState subscribes to lifecycle events for the instance and waits until
// it reaches targetState, a terminal/error state is detected, the timeout
// expires, or the context is cancelled. A polling fallback (every 5s) guards
// against missed or dropped events.
func WaitForState(ctx context.Context, mgr Manager, inst *Instance, targetState State, timeout time.Duration) (*WaitForStateResult, error) {
	// Subscribe first to avoid missing events between initial check and loop.
	ch, unsub := mgr.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
	defer unsub()

	// Already in target state — return immediately.
	if inst.State == targetState {
		return &WaitForStateResult{
			State:      inst.State,
			StateError: inst.StateError,
		}, nil
	}

	// Terminal or error state — won't reach target without explicit action.
	if isTerminalForWait(inst.State, targetState) {
		return &WaitForStateResult{
			State:      inst.State,
			StateError: inst.StateError,
		}, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(WaitForStatePollInterval)
	defer ticker.Stop()

	log := logger.FromContext(ctx)
	id := inst.Id
	latest := inst

	for {
		select {
		case <-ctx.Done():
			return &WaitForStateResult{
				State:      latest.State,
				StateError: latest.StateError,
				TimedOut:   true,
			}, nil

		case <-timer.C:
			got, err := mgr.GetInstance(ctx, id)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, ErrNotFound
				}
			} else {
				latest = got
			}
			return &WaitForStateResult{
				State:      latest.State,
				StateError: latest.StateError,
				TimedOut:   latest.State != targetState,
			}, nil

		case event, ok := <-ch:
			if !ok {
				return nil, ErrNotFound
			}
			if event.InstanceID != id {
				continue
			}
			if event.Action == LifecycleEventDelete {
				return nil, ErrNotFound
			}
			if event.Instance == nil {
				continue
			}
			latest = event.Instance

			if latest.State == targetState {
				return &WaitForStateResult{
					State:      latest.State,
					StateError: latest.StateError,
				}, nil
			}
			if isTerminalForWait(latest.State, targetState) {
				return &WaitForStateResult{
					State:      latest.State,
					StateError: latest.StateError,
				}, nil
			}

		case <-ticker.C:
			got, err := mgr.GetInstance(ctx, id)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, ErrNotFound
				}
				continue
			}
			latest = got

			if latest.State == targetState {
				log.DebugContext(ctx, "waitForState: state change detected via polling fallback",
					slog.String("instance_id", id),
					slog.String("target_state", string(targetState)),
				)
				return &WaitForStateResult{
					State:      latest.State,
					StateError: latest.StateError,
				}, nil
			}

			if isTerminalForWait(latest.State, targetState) {
				log.DebugContext(ctx, "waitForState: terminal state detected via polling fallback",
					slog.String("instance_id", id),
					slog.String("target_state", string(targetState)),
					slog.String("current_state", string(latest.State)),
				)
				return &WaitForStateResult{
					State:      latest.State,
					StateError: latest.StateError,
				}, nil
			}
		}
	}
}

// isTerminalForWait returns true if the current state won't progress toward
// the target without explicit user action (e.g. Stopped, Standby, Shutdown, Paused, Unknown).
func isTerminalForWait(current, target State) bool {
	if current == StateUnknown {
		return true
	}
	if current == StateStopped && target != StateStopped {
		return true
	}
	if current == StateStandby && target != StateStandby {
		return true
	}
	if current == StateShutdown && target != StateShutdown {
		return true
	}
	if current == StatePaused && target != StatePaused {
		return true
	}
	return false
}
