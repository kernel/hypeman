package healthcheck

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type ProbeRunner interface {
	Check(ctx context.Context, inst Instance, policy *Policy) ProbeResult
}

type ExecRunner interface {
	Run(ctx context.Context, inst Instance, check ExecCheck, timeout time.Duration) error
}

type DefaultProbeRunner struct {
	HTTPClient *http.Client
	ExecRunner ExecRunner
}

func (r DefaultProbeRunner) Check(ctx context.Context, inst Instance, policy *Policy) ProbeResult {
	switch policy.Type {
	case TypeHTTP:
		return r.checkHTTP(ctx, inst, *policy.HTTP)
	case TypeTCP:
		return r.checkTCP(ctx, inst, *policy.TCP)
	case TypeExec:
		return r.checkExec(ctx, inst, *policy.Exec, policy)
	default:
		return ProbeResult{Success: false, Error: "health check is disabled"}
	}
}

func (r DefaultProbeRunner) checkHTTP(ctx context.Context, inst Instance, check HTTPCheck) ProbeResult {
	if !inst.NetworkEnabled || inst.IP == "" {
		return ProbeResult{Success: false, Error: "instance has no network address"}
	}

	u := url.URL{
		Scheme: check.Scheme,
		Host:   net.JoinHostPort(inst.IP, strconv.Itoa(int(check.Port))),
		Path:   check.Path,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ProbeResult{Success: false, Error: err.Error()}
	}

	client := r.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{Success: false, Error: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != check.ExpectedStatus {
		return ProbeResult{
			Success: false,
			Error:   fmt.Sprintf("expected HTTP status %d, got %d", check.ExpectedStatus, resp.StatusCode),
		}
	}
	return ProbeResult{Success: true}
}

func (r DefaultProbeRunner) checkTCP(ctx context.Context, inst Instance, check TCPCheck) ProbeResult {
	if !inst.NetworkEnabled || inst.IP == "" {
		return ProbeResult{Success: false, Error: "instance has no network address"}
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(inst.IP, strconv.Itoa(int(check.Port))))
	if err != nil {
		return ProbeResult{Success: false, Error: err.Error()}
	}
	_ = conn.Close()
	return ProbeResult{Success: true}
}

func (r DefaultProbeRunner) checkExec(ctx context.Context, inst Instance, check ExecCheck, policy *Policy) ProbeResult {
	if r.ExecRunner == nil {
		return ProbeResult{Success: false, Error: "exec health checks are unavailable"}
	}
	if inst.SkipGuestAgent {
		return ProbeResult{Success: false, Error: "exec health checks require guest-agent"}
	}
	if !inst.GuestAgentReady {
		return ProbeResult{Success: false, Error: "guest-agent is not ready"}
	}

	_, timeout, _, err := DurationConfig(policy)
	if err != nil {
		return ProbeResult{Success: false, Error: err.Error()}
	}
	if err := r.ExecRunner.Run(ctx, inst, check, timeout); err != nil {
		return ProbeResult{Success: false, Error: err.Error()}
	}
	return ProbeResult{Success: true}
}
