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
	Runtime        *Runtime
}

// Connection is the normalized network view used by activity classification.
type Connection struct {
	OriginalSourceIP        netip.Addr
	OriginalSourcePort      uint16
	OriginalDestinationIP   netip.Addr
	OriginalDestinationPort uint16
	TCPState                TCPState
}

// Runtime stores persisted and in-memory idle-tracking timestamps.
type Runtime struct {
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

// Active reports whether the TCP state should keep a VM awake. Only states that
// mean the flow is finished stop counting; everything else is treated as inbound
// demand, including transient states conntrack reports on a live flow and any
// state this enum does not name.
//
// The asymmetry is deliberate. Wake-on-traffic does not exist, so a flow
// misjudged as finished is stranded until the client gives up, while a flow
// misjudged as live only costs idle VM time until the next DESTROY event or
// reconcile drops it. Enumerating live states instead kept missing them one at a
// time: SYN_SENT (a client mid-handshake) had to be added separately, and
// SYN_SENT2 — value 9, named TCPStateListen here — is a simultaneous open that
// was still classified as finished.
func (s TCPState) Active() bool {
	switch s {
	case TCPStateNone, TCPStateTimeWait, TCPStateClose:
		return false
	default:
		return true
	}
}

type compiledPolicy struct {
	idleTimeout       time.Duration
	ignoreSourceCIDRs []netip.Prefix
	ignorePorts       map[uint16]struct{}
}
