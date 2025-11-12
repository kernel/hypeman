package instances

import "fmt"

// ValidTransitions defines allowed single-hop state transitions
// Based on Cloud Hypervisor's actual state machine plus our additions
var ValidTransitions = map[State][]State{
	// Cloud Hypervisor native transitions
	StateCreated: {
		StateRunning,  // boot VM
		StateShutdown, // shutdown before boot
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
	case StateCreated, StateRunning, StatePaused, StateShutdown:
		return true
	case StateStopped, StateStandby:
		return false
	default:
		return false
	}
}


