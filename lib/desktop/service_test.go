package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureBackend struct {
	state  Status
	starts atomic.Int32
}

func (b *fixtureBackend) Status(context.Context) (Status, error) { return b.state, nil }
func (b *fixtureBackend) StartBrowser(context.Context) (Status, error) {
	b.starts.Add(1)
	s := b.state
	s.BrowserManaged = true
	s.BrowserReady = true
	s.Browser = "Chrome/test"
	return s, nil
}
func readyStatus() Status {
	return Status{Version: ProtocolVersion, OS: "darwin", Architecture: "arm64", UID: 501, ConsoleUID: 501, GUISession: true}
}

func TestDesktopRoleReadiness(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*Status)
		valid, ready bool
	}{
		{"ready", func(*Status) {}, true, true},
		{"logged out", func(s *Status) { s.ConsoleUID = 0 }, true, false},
		{"no GUI", func(s *Status) { s.GUISession = false }, true, false},
		{"wrong user", func(s *Status) { s.UID = 502 }, false, false},
		{"root", func(s *Status) { s.UID = 0 }, false, false},
		{"old version", func(s *Status) { s.Version = 0 }, false, false},
		{"unmanaged browser", func(s *Status) { s.BrowserReady = true }, false, true},
		{"browser without session", func(s *Status) { s.BrowserManaged = true; s.BrowserReady = true; s.GUISession = false }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := readyStatus()
			tc.change(&s)
			if (s.ValidateRole(501) == nil) != tc.valid {
				t.Fatal("role validation")
			}
			if tc.valid && s.SessionReady() != tc.ready {
				t.Fatal("session readiness")
			}
		})
	}
}

func TestDesktopServiceLaunchPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, path, method, body string
		loggedIn                 bool
		code, starts             int
	}{
		{"status", "/status", "GET", "", true, 200, 0},
		{"launch", "/browser/start", "POST", "", true, 200, 1},
		{"logout", "/browser/start", "POST", "", false, 409, 0},
		{"arguments", "/browser/start", "POST", "{\"args\":[\"--dangerous\"]}", true, 400, 0},
		{"query", "/browser/start?profile=other", "POST", "", true, 400, 0},
		{"shutdown unavailable", "/shutdown", "POST", "", true, 405, 0},
		{"CDP before browser ready", "/json/version", "GET", "", true, 409, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fixtureBackend{state: readyStatus()}
			b.state.GUISession = tc.loggedIn
			h, err := NewService(b, 501, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected CDP") }))
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tc.method, "http://guest"+tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.code || int(b.starts.Load()) != tc.starts {
				t.Fatalf("status=%d starts=%d", w.Code, b.starts.Load())
			}
		})
	}
}

func TestDesktopProbeBoundsAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{"valid", statusJSON(readyStatus()), true},
		{"wrong UID", strings.Replace(statusJSON(readyStatus()), `"uid":501`, `"uid":502`, 1), false},
		{"empty", "{}", false}, {"oversized", strings.Repeat(" ", maxStatusBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.URL.Path != "/status" {
					t.Error("unexpected probe")
				}
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer server.Close()
			_, err := Probe(context.Background(), testTransport(t, server), 501, false)
			if (err == nil) != tc.valid {
				t.Fatalf("probe: %v", err)
			}
		})
	}
}
func statusJSON(s Status) string { b, _ := json.Marshal(s); return string(b) }

type blockedLaunchBackend struct {
	fixtureBackend
	entered, release chan struct{}
}

func (b *blockedLaunchBackend) StartBrowser(ctx context.Context) (Status, error) {
	close(b.entered)
	select {
	case <-b.release:
		return b.fixtureBackend.StartBrowser(ctx)
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
}
func TestDesktopSerializesLaunches(t *testing.T) {
	b := &blockedLaunchBackend{fixtureBackend: fixtureBackend{state: readyStatus()}, entered: make(chan struct{}), release: make(chan struct{})}
	h, err := NewService(b, 501, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(first, httptest.NewRequest("POST", "http://guest/browser/start", nil).WithContext(ctx))
	}()
	select {
	case <-b.entered:
	case <-ctx.Done():
		t.Fatal("first launch did not start")
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("POST", "http://guest/browser/start", nil))
	if second.Code != 409 {
		t.Errorf("concurrent launch status: %d", second.Code)
	}
	close(b.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("launch did not finish")
	}
	if first.Code != 200 || b.starts.Load() != 1 {
		t.Fatalf("first launch status=%d starts=%d", first.Code, b.starts.Load())
	}
}

func TestDesktopProbeCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Probe(ctx, testTransport(t, server), 501, false); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("probe did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled probe succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not cancel")
	}
}

func TestDesktopOrigin(t *testing.T) {
	u, err := ParseOrigin("https://API.example:443/")
	if err != nil {
		t.Fatal(err)
	}
	if !SameOrigin("https://api.example:443", u) {
		t.Fatal("same origin rejected")
	}
	for _, bad := range []string{"null", "https://evil.example", "http://api.example:443", "https://api.example:443/path", "https://user@api.example:443", "https://api.example:443?", "https://api.example:443#x"} {
		if SameOrigin(bad, u) {
			t.Fatalf("accepted origin %s", bad)
		}
	}
	for _, bad := range []string{"ws://api.example", "https://user@api.example", "https://api.example/path", "https://api.example?", "https://api.example#x"} {
		if _, err := ParseOrigin(bad); err == nil {
			t.Fatalf("accepted config %s", bad)
		}
	}
}
