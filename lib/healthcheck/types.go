package healthcheck

import "time"

const (
	StateInitializing = "Initializing"
	StateRunning      = "Running"
)

type Type string

const (
	TypeNone Type = "none"
	TypeHTTP Type = "http"
	TypeTCP  Type = "tcp"
	TypeExec Type = "exec"
)

type Status string

const (
	StatusDisabled  Status = "disabled"
	StatusStarting  Status = "starting"
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
	StatusUnknown   Status = "unknown"
)

type Policy struct {
	Type             Type       `json:"type,omitempty"`
	Interval         string     `json:"interval,omitempty"`
	Timeout          string     `json:"timeout,omitempty"`
	StartPeriod      string     `json:"start_period,omitempty"`
	FailureThreshold int        `json:"failure_threshold,omitempty"`
	SuccessThreshold int        `json:"success_threshold,omitempty"`
	HTTP             *HTTPCheck `json:"http,omitempty"`
	TCP              *TCPCheck  `json:"tcp,omitempty"`
	Exec             *ExecCheck `json:"exec,omitempty"`
}

type HTTPCheck struct {
	Port           uint16 `json:"port"`
	Path           string `json:"path,omitempty"`
	Scheme         string `json:"scheme,omitempty"`
	ExpectedStatus int    `json:"expected_status,omitempty"`
}

type TCPCheck struct {
	Port uint16 `json:"port"`
}

type ExecCheck struct {
	Command    []string `json:"command,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
}

type Runtime struct {
	Status               Status     `json:"status,omitempty"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	ConsecutiveSuccesses int        `json:"consecutive_successes,omitempty"`
	ConsecutiveFailures  int        `json:"consecutive_failures,omitempty"`
	LastCheckedAt        *time.Time `json:"last_checked_at,omitempty"`
	LastSuccessAt        *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt        *time.Time `json:"last_failure_at,omitempty"`
	LastError            string     `json:"last_error,omitempty"`
}

type StatusSnapshot struct {
	Status               Status
	ConsecutiveSuccesses int
	ConsecutiveFailures  int
	LastCheckedAt        *time.Time
	LastSuccessAt        *time.Time
	LastFailureAt        *time.Time
	LastError            string
}

type Instance struct {
	ID              string
	Name            string
	State           string
	NetworkEnabled  bool
	IP              string
	StartedAt       *time.Time
	GuestAgentReady bool
	SkipGuestAgent  bool
	HealthCheck     *Policy
	Runtime         *Runtime
}
