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
	ID             string
	Name           string
	State          string
	NetworkEnabled bool
	IP             string
	HasVGPU        bool
	AutoStandby    *Policy
}

// Connection is the normalized network view used by activity classification.
type Connection struct {
	OriginalSourceIP        netip.Addr
	OriginalDestinationIP   netip.Addr
	OriginalDestinationPort uint16
	TCPState                TCPState
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
	TCPStateIgnore      TCPState = 11
)

// Active reports whether the TCP state should keep a VM awake.
func (s TCPState) Active() bool {
	switch s {
	case TCPStateSynRecv, TCPStateEstablished, TCPStateFinWait, TCPStateCloseWait, TCPStateLastAck:
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
