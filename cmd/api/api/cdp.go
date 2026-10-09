package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kernel/hypeman/lib/desktop"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
)

// CDPHandler requires instance-write authentication and resolution in the router.
func (s *ApiService) CDPHandler(w http.ResponseWriter, r *http.Request) {
	s.serveDesktop(w, r, newDesktopTransport)
}

func newDesktopTransport(inst *instances.Instance) (*http.Transport, error) {
	dialer, err := hypervisor.NewVsockDialer(hypervisor.Type(inst.HypervisorType), inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return nil, err
	}
	return &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 20 * time.Second, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialVsock(ctx, int(desktop.AgentPort))
	}}, nil
}

func (s *ApiService) serveDesktop(w http.ResponseWriter, r *http.Request, transportFor func(*instances.Instance) (*http.Transport, error)) {
	inst := mw.GetResolvedInstance[instances.Instance](r.Context())
	if inst == nil {
		http.Error(w, "instance not resolved", 500)
		return
	}
	if inst.MacOS == nil || hypervisor.Type(inst.HypervisorType) != hypervisor.TypeVZ || inst.MacOS.DesktopAgentUID == 0 || inst.SkipGuestAgent {
		http.Error(w, "instance does not declare an enabled macOS desktop agent", 501)
		return
	}
	if inst.State != instances.StateRunning {
		http.Error(w, "instance must be running", 409)
		return
	}
	if s.Config == nil || s.Config.MacOSDesktopOrigin == "" {
		http.Error(w, "desktop API origin not configured", 503)
		return
	}
	origin, err := desktop.ParseOrigin(s.Config.MacOSDesktopOrigin)
	if err != nil {
		http.Error(w, "invalid desktop API origin", 503)
		return
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 || len(origins) == 1 && !desktop.SameOrigin(origins[0], origin) {
		http.Error(w, "cross-origin desktop access rejected", 403)
		return
	}
	selector := chi.URLParam(r, "id")
	if selector == "" {
		selector = inst.Id
	}
	prefix := "/instances/" + selector + "/cdp"
	if !strings.HasPrefix(r.URL.Path, prefix+"/") || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "invalid desktop request", 400)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	status := path == "/status" && r.Method == http.MethodGet
	start := path == "/start" && r.Method == http.MethodPost
	request := r.Clone(r.Context())
	request.URL.Path = path
	if !status && !start {
		if err := desktop.ValidateCDPRequest(request); err != nil {
			http.Error(w, "unsupported CDP request", 400)
			return
		}
	}
	s.desktopSlotsOnce.Do(func() { s.desktopSlots = make(chan struct{}, desktop.MaxSessions) })
	select {
	case s.desktopSlots <- struct{}{}:
		defer func() { <-s.desktopSlots }()
	default:
		http.Error(w, "desktop session limit reached", http.StatusTooManyRequests)
		return
	}
	transport, err := transportFor(inst)
	if err != nil {
		http.Error(w, "desktop transport unavailable", 503)
		return
	}
	defer transport.CloseIdleConnections()
	timeout := 3 * time.Second
	if start {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	state, err := desktop.Probe(ctx, transport, inst.MacOS.DesktopAgentUID, start)
	cancel()
	if err != nil {
		http.Error(w, "selected desktop agent unavailable or incompatible", 503)
		return
	}
	if status || start {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			desktop.Status
			DesktopReady bool `json:"desktop_ready"`
		}{state, state.SessionReady()})
		return
	}
	if !state.BrowserReady {
		http.Error(w, "selected desktop session or managed browser not ready", 409)
		return
	}
	scheme := "ws"
	if origin.Scheme == "https" {
		scheme = "wss"
	}
	base := scheme + "://" + origin.Host + "/instances/" + url.PathEscape(inst.Id) + "/cdp"
	proxy, err := desktop.NewCDPProxy(transport, base)
	if err != nil {
		http.Error(w, "invalid instance CDP configuration", 503)
		return
	}
	proxy.ServeHTTP(w, request)
}
