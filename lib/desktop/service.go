package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	ProtocolVersion        = 1
	MaxSessions            = 16
	AgentPort       uint32 = 2223
	maxStatusBytes         = 8 << 10
)

// Status reports the selected user session, not VMM or root-service readiness.
// BrowserManaged means the desktop agent established ownership of this browser.
// Merely finding an open loopback debugging port must not set BrowserManaged.
type Status struct {
	Version        int    `json:"version"`
	OS             string `json:"os"`
	Architecture   string `json:"architecture"`
	UID            uint32 `json:"uid"`
	ConsoleUID     uint32 `json:"console_uid"`
	GUISession     bool   `json:"gui_session"`
	BrowserManaged bool   `json:"browser_managed"`
	BrowserReady   bool   `json:"browser_ready"`
	Browser        string `json:"browser,omitempty"`
}

func (s Status) ValidateRole(uid uint32) error {
	if s.Version != ProtocolVersion || s.OS != "darwin" || s.Architecture != "arm64" || uid == 0 || s.UID != uid {
		return fmt.Errorf("incompatible desktop agent or user")
	}
	if s.BrowserReady && (!s.BrowserManaged || !s.SessionReady()) {
		return fmt.Errorf("inconsistent browser readiness")
	}
	return nil
}

func (s Status) SessionReady() bool { return s.UID != 0 && s.ConsoleUID == s.UID && s.GUISession }

// Backend must implement fixed browser provisioning, not client-selected commands,
// paths, arguments or profiles. System shutdown and arbitrary exec are not here.
type Backend interface {
	Status(context.Context) (Status, error)
	StartBrowser(context.Context) (Status, error)
}

// NewService is the guest desktop role's HTTP interface. Its listener must admit
// host-CID connections only; it must never be published on a guest TCP interface.
// uid selects the provisioned non-root user and does not come from the request.
func NewService(backend Backend, uid uint32, cdp http.Handler) (http.Handler, error) {
	if backend == nil || uid == 0 || cdp == nil {
		return nil, fmt.Errorf("desktop backend, non-root user and CDP handler required")
	}
	launching := make(chan struct{}, 1)
	sessions := make(chan struct{}, MaxSessions)
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, s Status) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s)
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "invalid status request", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		s, err := backend.Status(ctx)
		if err != nil || s.ValidateRole(uid) != nil {
			http.Error(w, "desktop status unavailable", 503)
			return
		}
		respond(w, s)
	})
	mux.HandleFunc("POST /browser/start", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "browser launch arguments unsupported", 400)
			return
		}
		select {
		case launching <- struct{}{}:
			defer func() { <-launching }()
		default:
			http.Error(w, "browser launch in progress", 409)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		s, err := backend.Status(ctx)
		if err != nil || s.ValidateRole(uid) != nil {
			http.Error(w, "desktop status unavailable", 503)
			return
		}
		if !s.SessionReady() {
			http.Error(w, "selected desktop session not ready", 409)
			return
		}
		if !s.BrowserReady {
			s, err = backend.StartBrowser(ctx)
		}
		if err != nil || s.ValidateRole(uid) != nil || !s.BrowserReady {
			http.Error(w, "managed browser not ready", 503)
			return
		}
		respond(w, s)
	})
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := ValidateCDPRequest(r); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		select {
		case sessions <- struct{}{}:
			defer func() { <-sessions }()
		default:
			http.Error(w, "desktop session limit reached", 429)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		s, err := backend.Status(ctx)
		cancel()
		if err != nil || s.ValidateRole(uid) != nil || !s.BrowserReady {
			http.Error(w, "managed browser not ready", 409)
			return
		}
		cdp.ServeHTTP(w, r)
	}))
	return mux, nil
}

// Probe validates a bounded desktop role handshake over the supplied transport.
// The request carries no API headers or caller-selected endpoint.
func Probe(ctx context.Context, transport http.RoundTripper, uid uint32, start bool) (Status, error) {
	if transport == nil {
		return Status{}, errors.New("desktop transport required")
	}
	method, path := http.MethodGet, "/status"
	if start {
		method, path = http.MethodPost, "/browser/start"
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://desktop"+path, nil)
	if err != nil {
		return Status{}, err
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return Status{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("desktop agent returned %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxStatusBytes+1))
	if err != nil {
		return Status{}, err
	}
	if len(data) > maxStatusBytes {
		return Status{}, errors.New("desktop status too large")
	}
	var s Status
	if err = json.Unmarshal(data, &s); err != nil {
		return Status{}, err
	}
	if err = s.ValidateRole(uid); err != nil {
		return Status{}, err
	}
	if start && !s.BrowserReady {
		return Status{}, errors.New("browser launch did not establish readiness")
	}
	return s, nil
}
