package snapshot

import "fmt"

const (
	StateStopped = "Stopped"
	StateStandby = "Standby"
	StateRunning = "Running"
)

// ResolveTargetState applies snapshot defaults and validates requested target
// states for restore/fork flows.
func ResolveTargetState(kind SnapshotKind, requested string) (string, error) {
	if requested == "" {
		switch kind {
		case SnapshotKindStandby:
			return StateRunning, nil
		case SnapshotKindStopped:
			return StateStopped, nil
		default:
			return "", fmt.Errorf("unsupported snapshot kind %q", kind)
		}
	}

	switch kind {
	case SnapshotKindStandby:
		if requested == StateRunning || requested == StateStandby || requested == StateStopped {
			return requested, nil
		}
	case SnapshotKindStopped:
		if requested == StateStopped || requested == StateRunning {
			return requested, nil
		}
	}

	return "", fmt.Errorf("invalid target_state %q for snapshot kind %s", requested, kind)
}
