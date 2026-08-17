package autostandby

import (
	"net/netip"
	"time"
)

const (
	StateRunning = "Running"
)

// Policy configures per-instance automatic standby behavior.
type Policy struct {
	Enabled                bool     `json:"enabled"`
	IdleTimeout            string   `json:"idle_timeout,omitempty"`
	IgnoreSourceCIDRs      []string `json:"ignore_source_cidrs,omitempty"`
	IgnoreDestinationPorts []uint16 `json:"ignore_destination_ports,omitempty"`
}

// Instance is the minimal instance view needed by the auto-standby controller.
type Instance struct {
	ID               string
	Name             string
	State            string
	NetworkEnabled   bool
	IP               string
	HasVGPU          bool
	AutoStandby      *Policy
	AutoStandbyState *AutoStandbyState
}

// Connection is the normalized network view used by activity classification.
type Connection struct {
	OriginalSourceIP        netip.Addr
	OriginalSourcePort      uint16
	OriginalDestinationIP   netip.Addr
	OriginalDestinationPort uint16
	TCPState                TCPState
}

// AutoStandbyState stores persisted and in-memory idle-tracking timestamps.
type AutoStandbyState struct {
	IdleSince             *time.Time `json:"idle_since,omitempty"`
	LastInboundActivityAt *time.Time `json:"last_inbound_activity_at,omitempty"`
}

// TCPState is the conntrack TCP state for a flow.
type TCPState uint8

const (
	TCPStateNone        TCPState = 0
	TCPStateSynSent     TCPState = 1
	TCPStateSynRecv     TCPState = 2
	TCPStateEstablished TCPState = 3
	TCPStateFinWait     TCPState = 4
	TCPStateCloseWait   TCPState = 5
	TCPStateLastAck     TCPState = 6
	TCPStateTimeWait    TCPState = 7
	TCPStateClose       TCPState = 8
	TCPStateListen      TCPState = 9
	TCPStateIgnore      TCPState = 10
	TCPStateRetrans     TCPState = 11
)

// Active reports whether the TCP state should keep a VM awake. SYN_SENT
// counts: a client mid-handshake is inbound demand, and a freshly restored
// guest can take several seconds to answer the SYN it was woken for — going
// back to standby in that window orphans the connection, since wake-on-traffic
// does not exist.
func (s TCPState) Active() bool {
	switch s {
	case TCPStateSynSent, TCPStateSynRecv, TCPStateEstablished, TCPStateFinWait, TCPStateCloseWait, TCPStateLastAck:
		return true
	default:
		return false
	}
}

type compiledPolicy struct {
	idleTimeout       time.Duration
	ignoreSourceCIDRs []netip.Prefix
	ignorePorts       map[uint16]struct{}
}
