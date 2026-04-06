package autostandby

import "time"

type Status string

const (
	StatusUnsupported    Status = "unsupported"
	StatusDisabled       Status = "disabled"
	StatusIneligible     Status = "ineligible"
	StatusActive         Status = "active"
	StatusIdleCountdown  Status = "idle_countdown"
	StatusReadyForStandby Status = "ready_for_standby"
	StatusStandbyRequested Status = "standby_requested"
	StatusError          Status = "error"
)

type Reason string

const (
	ReasonUnsupportedPlatform   Reason = "unsupported_platform"
	ReasonPolicyMissing         Reason = "policy_missing"
	ReasonPolicyDisabled        Reason = "policy_disabled"
	ReasonInstanceNotRunning    Reason = "instance_not_running"
	ReasonNetworkDisabled       Reason = "network_disabled"
	ReasonMissingIP             Reason = "missing_ip"
	ReasonHasVGPU               Reason = "has_vgpu"
	ReasonActiveInbound         Reason = "active_inbound_connections"
	ReasonIdleTimeoutNotElapsed Reason = "idle_timeout_not_elapsed"
	ReasonObserverError         Reason = "observer_error"
	ReasonReadyForStandby       Reason = "ready_for_standby"
)

// StatusSnapshot is a diagnostic view of the controller's current state for one VM.
type StatusSnapshot struct {
	Supported             bool
	Configured            bool
	Enabled               bool
	Eligible              bool
	Status                Status
	Reason                Reason
	ActiveInboundCount    int
	IdleTimeout           string
	IdleSince             *time.Time
	LastInboundActivityAt *time.Time
	NextStandbyAt         *time.Time
	CountdownRemaining    *time.Duration
	TrackingMode          string
}

