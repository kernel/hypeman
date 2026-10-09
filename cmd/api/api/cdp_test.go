package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/desktop"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/stretchr/testify/require"
)

type desktopInstances struct {
	instances.Manager
	inst    *instances.Instance
	lookups atomic.Int32
}

func (m *desktopInstances) GetInstance(_ context.Context, name string) (*instances.Instance, error) {
	m.lookups.Add(1)
	if name != "test" && name != "alias" {
		return nil, instances.ErrNotFound
	}
	return m.inst, nil
}
func desktopInstance() *instances.Instance {
	return &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "test", HypervisorType: "vz", MacOS: &images.MacOSImage{DesktopAgentUID: 501}}, State: instances.StateRunning}
}
func desktopToken(t *testing.T, permission string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "test", "exp": time.Now().Add(time.Hour).Unix(), "permissions": []string{permission}}).SignedString([]byte("synthetic-test-secret"))
	require.NoError(t, err)
	return s
}
func desktopRouter(s *ApiService, factory func(*instances.Instance) (*http.Transport, error)) http.Handler {
	r := chi.NewRouter()
	sub := r.With(mw.JwtAuth("synthetic-test-secret"), scopes.RequireScope(scopes.InstanceWrite), mw.ResolveResource(s.NewResolvers(), ResolverErrorResponder))
	h := func(w http.ResponseWriter, r *http.Request) { s.serveDesktop(w, r, factory) }
	sub.Get("/instances/{id}/cdp/*", h)
	sub.Post("/instances/{id}/cdp/start", h)
	return r
}

func TestDesktopAdmissionBeforeDial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*instances.Instance, *config.Config, *http.Request)
		code   int
	}{
		{"missing token", func(_ *instances.Instance, _ *config.Config, r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"read-only", func(_ *instances.Instance, _ *config.Config, r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+desktopToken(t, string(scopes.InstanceRead)))
		}, 403},
		{"undeclared", func(i *instances.Instance, _ *config.Config, _ *http.Request) { i.MacOS.DesktopAgentUID = 0 }, 501},
		{"disabled", func(i *instances.Instance, _ *config.Config, _ *http.Request) { i.SkipGuestAgent = true }, 501},
		{"stopped", func(i *instances.Instance, _ *config.Config, _ *http.Request) { i.State = instances.StateStopped }, 409},
		{"unknown instance", func(_ *instances.Instance, _ *config.Config, r *http.Request) {
			r.URL.Path = "/instances/other/cdp/json/version"
		}, 404},
		{"no origin config", func(_ *instances.Instance, c *config.Config, _ *http.Request) { c.MacOSDesktopOrigin = "" }, 503},
		{"wrong origin", func(_ *instances.Instance, _ *config.Config, r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
		}, 403},
		{"multiple origins", func(_ *instances.Instance, _ *config.Config, r *http.Request) {
			r.Header.Add("Origin", "https://api.example")
			r.Header.Add("Origin", "https://api.example")
		}, 403},
		{"unsafe discovery", func(_ *instances.Instance, _ *config.Config, r *http.Request) {
			r.URL.Path = "/instances/test/cdp/json/new"
		}, 400},
		{"query", func(_ *instances.Instance, _ *config.Config, r *http.Request) { r.URL.RawQuery = "token=secret" }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := desktopInstance()
			c := &config.Config{MacOSDesktopOrigin: "https://api.example"}
			m := &desktopInstances{inst: i}
			s := &ApiService{Config: c, InstanceManager: m}
			r := httptest.NewRequest("GET", "http://spoof.example/instances/test/cdp/json/version", nil)
			r.Header.Set("Authorization", "Bearer "+desktopToken(t, string(scopes.InstanceWrite)))
			tc.change(i, c, r)
			w := httptest.NewRecorder()
			desktopRouter(s, func(*instances.Instance) (*http.Transport, error) { t.Fatal("dial before admission"); return nil, nil }).ServeHTTP(w, r)
			require.Equal(t, tc.code, w.Code, w.Body.String())
			if tc.code == 401 || tc.name == "read-only" {
				require.Zero(t, m.lookups.Load())
			}
		})
	}
}

func TestDesktopHandshakeAndDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		uid   uint32
		ready bool
		code  int
	}{{"ready", 501, true, 200}, {"wrong desktop user", 502, true, 503}, {"root desktop user", 0, true, 503}, {"browser not ready", 501, false, 409}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Empty(t, r.Header.Get("Authorization"))
				require.Empty(t, r.Header.Get("Cookie"))
				require.Empty(t, r.Header.Get("Forwarded"))
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/status" {
					_ = json.NewEncoder(w).Encode(desktop.Status{Version: 1, OS: "darwin", Architecture: "arm64", UID: tc.uid, ConsoleUID: tc.uid, GUISession: true, BrowserManaged: tc.ready, BrowserReady: tc.ready})
					return
				}
				require.Equal(t, "/json/version", r.URL.Path)
				_, _ = w.Write([]byte(`{"Browser":"Chrome/test","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/test-id"}`))
			}))
			defer guest.Close()
			m := &desktopInstances{inst: desktopInstance()}
			s := &ApiService{Config: &config.Config{MacOSDesktopOrigin: "https://api.example"}, InstanceManager: m}
			factory := func(*instances.Instance) (*http.Transport, error) {
				return &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(guest.URL, "http://"))
				}}, nil
			}
			r := httptest.NewRequest("GET", "http://spoof.example/instances/alias/cdp/json/version", nil)
			r.Header.Set("Authorization", "Bearer "+desktopToken(t, string(scopes.InstanceWrite)))
			r.Header.Set("Cookie", "secret=value")
			r.Header.Set("Forwarded", "host=evil.example")
			r.Header.Set("Origin", "https://api.example")
			w := httptest.NewRecorder()
			desktopRouter(s, factory).ServeHTTP(w, r)
			require.Equal(t, tc.code, w.Code, w.Body.String())
			if tc.code == 200 {
				require.Contains(t, w.Body.String(), "wss://api.example/instances/test/cdp/devtools/browser/test-id")
				require.Equal(t, int32(2), calls.Load())
			} else {
				require.Equal(t, int32(1), calls.Load())
			}
		})
	}
}
