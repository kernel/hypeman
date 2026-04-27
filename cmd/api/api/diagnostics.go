package api

import (
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
)

// dnsProbeHost is the external hostname resolved during the DNS check.
// kernel.sh is owned by Kernel and answered by stable infrastructure;
// resolving it tells us the host's stub resolver and uplink DNS can answer
// real-world queries (not just localhost).
const dnsProbeHost = "kernel.sh"

// egressProbeAddr is the TCP target used to verify outbound network reach.
// 1.1.1.1:443 is operated by Cloudflare and accepts TCP/443 from anywhere
// reachable on the public internet, so a successful dial confirms the host
// can leave its uplink without DNS dependency.
const egressProbeAddr = "1.1.1.1:443"

// minFreeDiskBytes is the threshold below which the disk check fails.
// 5 GiB leaves headroom for image pulls, snapshots, and overlay growth on
// hosts already running close to capacity.
const minFreeDiskBytes uint64 = 5 * 1024 * 1024 * 1024

const probeTimeout = 3 * time.Second

// GetDiagnostics runs synchronous host probes and returns per-check results.
func (s *ApiService) GetDiagnostics(ctx context.Context, _ oapi.GetDiagnosticsRequestObject) (oapi.GetDiagnosticsResponseObject, error) {
	checks := runDiagnosticChecks(ctx, s.Config.DataDir)

	allOK := true
	for _, c := range checks {
		if !c.Ok {
			allOK = false
			break
		}
	}

	if !allOK {
		log := logger.FromContext(ctx)
		log.WarnContext(ctx, "diagnostics reported failures", "checks", checks)
	}

	return oapi.GetDiagnostics200JSONResponse{
		Ok:        allOK,
		CheckedAt: time.Now().UTC(),
		Checks:    checks,
	}, nil
}

// runDiagnosticChecks runs all probes in parallel and returns results in a
// stable order so dashboards/alerts can index by position.
func runDiagnosticChecks(ctx context.Context, dataDir string) []oapi.DiagnosticCheck {
	results := make([]oapi.DiagnosticCheck, 3)
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		results[0] = probeDNS(ctx)
	}()
	go func() {
		defer wg.Done()
		results[1] = probeEgressTCP(ctx)
	}()
	go func() {
		defer wg.Done()
		results[2] = probeDisk(dataDir)
	}()

	wg.Wait()
	return results
}

func probeDNS(ctx context.Context) oapi.DiagnosticCheck {
	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(probeCtx, dnsProbeHost)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		errMsg := err.Error()
		return oapi.DiagnosticCheck{
			Name:       "dns",
			Ok:         false,
			DurationMs: dur,
			Error:      &errMsg,
		}
	}
	if len(addrs) == 0 {
		errMsg := "no addresses returned"
		return oapi.DiagnosticCheck{
			Name:       "dns",
			Ok:         false,
			DurationMs: dur,
			Error:      &errMsg,
		}
	}
	detail := fmt.Sprintf("%s -> %s", dnsProbeHost, addrs[0])
	return oapi.DiagnosticCheck{
		Name:       "dns",
		Ok:         true,
		DurationMs: dur,
		Detail:     &detail,
	}
}

func probeEgressTCP(ctx context.Context) oapi.DiagnosticCheck {
	start := time.Now()
	d := net.Dialer{Timeout: probeTimeout}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	conn, err := d.DialContext(probeCtx, "tcp", egressProbeAddr)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		errMsg := err.Error()
		return oapi.DiagnosticCheck{
			Name:       "egress_tcp",
			Ok:         false,
			DurationMs: dur,
			Error:      &errMsg,
		}
	}
	_ = conn.Close()
	detail := egressProbeAddr
	return oapi.DiagnosticCheck{
		Name:       "egress_tcp",
		Ok:         true,
		DurationMs: dur,
		Detail:     &detail,
	}
}

func probeDisk(dataDir string) oapi.DiagnosticCheck {
	start := time.Now()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &stat); err != nil {
		dur := time.Since(start).Milliseconds()
		errMsg := err.Error()
		return oapi.DiagnosticCheck{
			Name:       "disk",
			Ok:         false,
			DurationMs: dur,
			Error:      &errMsg,
		}
	}
	free := stat.Bavail * uint64(stat.Bsize)
	dur := time.Since(start).Milliseconds()
	detail := fmt.Sprintf("free=%d bytes path=%s", free, dataDir)
	if free < minFreeDiskBytes {
		errMsg := fmt.Sprintf("free disk %d below threshold %d", free, minFreeDiskBytes)
		return oapi.DiagnosticCheck{
			Name:       "disk",
			Ok:         false,
			DurationMs: dur,
			Error:      &errMsg,
			Detail:     &detail,
		}
	}
	return oapi.DiagnosticCheck{
		Name:       "disk",
		Ok:         true,
		DurationMs: dur,
		Detail:     &detail,
	}
}
