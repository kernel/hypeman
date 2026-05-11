package instances

import (
	"fmt"
	"strings"
)

// userActionsByState lists the user-facing API actions a caller can invoke
// when the instance is in the given state. Used to build self-describing
// ErrInvalidState messages.
var userActionsByState = map[State][]string{
	StateInitializing: {"stop", "update"},
	StateRunning:      {"stop", "standby", "snapshot", "fork (with from_running=true)", "update"},
	StateStandby:      {"restore", "fork", "snapshot", "restore_snapshot"},
	StateStopped:      {"start", "fork", "snapshot", "restore_snapshot"},
}

// UserActions returns the user-facing API actions a caller can invoke from
// the current state. Returns nil for states with no caller-invocable actions
// (transient states like Created/Paused/Shutdown, or StateUnknown).
func (s State) UserActions() []string {
	actions := userActionsByState[s]
	if len(actions) == 0 {
		return nil
	}
	out := make([]string, len(actions))
	copy(out, actions)
	return out
}

// NewInvalidStateError builds an ErrInvalidState that names the attempted
// action, the current state, and the actions valid from the current state.
// Use this for caller-facing state-rejection errors so the response tells
// the caller what they can do next.
func NewInvalidStateError(action string, current State) error {
	actions := current.UserActions()
	if len(actions) == 0 {
		return fmt.Errorf("%w: cannot %s from state %s (no actions valid from this state)", ErrInvalidState, action, current)
	}
	return fmt.Errorf("%w: cannot %s from state %s, valid actions from %s: %s", ErrInvalidState, action, current, current, strings.Join(actions, ", "))
}

// ValidTransitions defines allowed single-hop state transitions
// Based on Cloud Hypervisor's actual state machine plus our additions
var ValidTransitions = map[State][]State{
	// Cloud Hypervisor native transitions
	StateCreated: {
		StateInitializing, // boot VM (guest init in progress)
		StateRunning,      // boot VM (fast path; markers already available)
		StateShutdown,     // shutdown before boot
	},
	StateInitializing: {
		StateRunning,  // guest init complete
		StateShutdown, // shutdown
	},
	StateRunning: {
		StatePaused,   // pause
		StateShutdown, // shutdown
	},
	StatePaused: {
		StateRunning,  // resume
		StateShutdown, // shutdown while paused
		StateStandby,  // snapshot + kill VMM (atomic operation)
	},
	StateShutdown: {
		StateRunning, // restart
		StateStopped, // cleanup VMM (terminal)
	},

	// Our additional transitions
	StateStopped: {
		StateCreated, // start VMM process
	},
	StateStandby: {
		StatePaused,  // start VMM + restore (atomic operation)
		StateStopped, // delete snapshot + cleanup (terminal)
	},
	// StateUnknown means we failed to determine state - no transitions allowed.
	// Operations on instances in Unknown state should fail with an error
	// until the underlying issue is resolved.
	// Can still Delete the instance.
	StateUnknown: {},
}

// CanTransitionTo checks if a transition from current state to target state is valid
func (s State) CanTransitionTo(target State) error {
	allowed, ok := ValidTransitions[s]
	if !ok {
		return fmt.Errorf("%w: unknown state: %s", ErrInvalidState, s)
	}

	for _, valid := range allowed {
		if valid == target {
			return nil
		}
	}

	return fmt.Errorf("%w: cannot transition from %s to %s", ErrInvalidState, s, target)
}

// String returns the string representation of the state
func (s State) String() string {
	return string(s)
}

// IsTerminal returns true if this state represents a terminal transition point
func (s State) IsTerminal() bool {
	return s == StateStopped
}

// RequiresVMM returns true if this state requires a running VMM process
func (s State) RequiresVMM() bool {
	switch s {
	case StateCreated, StateInitializing, StateRunning, StatePaused, StateShutdown:
		return true
	case StateStopped, StateStandby, StateUnknown:
		return false
	default:
		return false
	}
}
