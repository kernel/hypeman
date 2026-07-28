package autostandby

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTCPStateConstantsMatchExpectedKernelValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, TCPState(10), TCPStateIgnore)
	assert.Equal(t, TCPState(11), TCPStateRetrans)
}

func TestActiveOnlyTreatsFinishedFlowsAsIdle(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state TCPState
		want  bool
	}{
		{"none", TCPStateNone, false},
		{"time_wait", TCPStateTimeWait, false},
		{"close", TCPStateClose, false},
		{"syn_sent", TCPStateSynSent, true},
		{"syn_recv", TCPStateSynRecv, true},
		{"established", TCPStateEstablished, true},
		{"fin_wait", TCPStateFinWait, true},
		{"close_wait", TCPStateCloseWait, true},
		{"last_ack", TCPStateLastAck, true},
		// SYN_SENT2: a simultaneous open is still a client waiting on the guest.
		{"syn_sent2", TCPStateListen, true},
		{"ignore", TCPStateIgnore, true},
		{"retrans", TCPStateRetrans, true},
		// Anything this enum does not name has to count, or the next state the
		// kernel reports on a live flow strands it again.
		{"unnamed", TCPState(12), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.state.Active())
		})
	}
}
