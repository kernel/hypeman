package desktop

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCDPDiscoveryAndHeaderIsolation(t *testing.T) {
	for _, payload := range []string{
		`{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc-123","devtoolsFrontendUrl":"unsafe"}`,
		`[{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/ABC123"}]`,
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != "127.0.0.1:9222" || r.URL.Path != "/json/version" {
				t.Errorf("unexpected upstream: %s %s", r.Host, r.URL)
			}
			for _, key := range []string{"Authorization", "Cookie", "Origin", "Proxy-Authorization", "X-API-Key", "X-Forwarded-Host", "X-Forwarded-For"} {
				if r.Header.Get(key) != "" {
					t.Errorf("credential/header forwarded: %s", key)
				}
			}
			w.Header().Set("Set-Cookie", "guest-secret=yes")
			_, _ = io.WriteString(w, payload)
		}))
		transport := testTransport(t, upstream)
		handler, err := NewCDPProxy(transport, "wss://api.example/instances/id/cdp")
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "http://private:secret@attacker.example/json/version", nil)
		for _, key := range []string{"Authorization", "Cookie", "Origin", "Proxy-Authorization", "X-API-Key", "X-Forwarded-Host", "X-Forwarded-For"} {
			req.Header.Set(key, "secret")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "wss://api.example/instances/id/cdp/devtools/") {
			t.Fatalf("discovery: %d %s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "unsafe") || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("unsafe discovery response")
		}
		upstream.Close()
	}
}

func testTransport(t *testing.T, upstream *httptest.Server) *http.Transport {
	t.Helper()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func TestCDPRejectsUnsupportedRequestsBeforeDial(t *testing.T) {
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("unsupported request reached upstream")
		return nil, nil
	}}
	h, err := NewCDPProxy(tr, "ws://api.example/instances/id/cdp")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/json/version"}, {"PUT", "/json/new"}, {"GET", "/json/new"}, {"GET", "/json/close/id"}, {"GET", "/status"}, {"GET", "/json/version?url=http://evil"}, {"GET", "/json%2fversion"}, {"GET", "/devtools/browser/../id"}, {"GET", "/devtools/browser/id"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, "http://api.example"+tc.path, nil))
			if w.Code != 400 {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
}

func TestCDPRejectsUntrustedDiscovery(t *testing.T) {
	for _, payload := range []string{
		`{"webSocketDebuggerUrl":"ws://evil.example/devtools/browser/id"}`,
		`{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/id?token=secret"}`,
		`{"webSocketDebuggerUrl":"ws://user@127.0.0.1:9222/devtools/browser/id"}`,
		`{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/id#fragment"}`,
		`{"webSocketDebuggerUrl":42}`, `null`, `[42]`, `not json`, strings.Repeat(" ", maxDiscoveryBytes+1),
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, payload) }))
		h, err := NewCDPProxy(testTransport(t, upstream), "ws://api.example/instances/id/cdp")
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/json/version", nil))
		if w.Code != 502 {
			t.Fatalf("invalid discovery accepted: %d", w.Code)
		}
		upstream.Close()
	}
}

func TestCDPRejectsRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://evil.example", 302) }))
	defer upstream.Close()
	h, err := NewCDPProxy(testTransport(t, upstream), "ws://api.example/instances/id/cdp")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/json/version", nil))
	if w.Code != 502 {
		t.Fatalf("redirect status %d", w.Code)
	}
}

func TestCDPRejectsDiscoveryUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer upstream.Close()
	h, err := NewCDPProxy(testTransport(t, upstream), "ws://api.example/instances/id/cdp")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/json/version", nil))
	if w.Code != 502 {
		t.Fatalf("unexpected discovery upgrade: %d", w.Code)
	}
}

func TestCDPWebSocketRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devtools/browser/abc-123" || r.Header.Get("Authorization") != "" {
			t.Error("invalid WebSocket upstream request")
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		kind, data, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(kind, data)
		}
	}))
	defer upstream.Close()
	h, err := NewCDPProxy(testTransport(t, upstream), "ws://api.example/instances/id/cdp")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(h)
	defer proxy.Close()
	headers := http.Header{"Authorization": []string{"Bearer private"}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/devtools/browser/abc-123", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"method":"Browser.getVersion"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil || string(data) != `{"id":1,"method":"Browser.getVersion"}` {
		t.Fatalf("WebSocket round trip failed: %s %v", data, err)
	}
}

func TestCDPRejectsRequestBody(t *testing.T) {
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("request body reached upstream")
		return nil, nil
	}}
	h, err := NewCDPProxy(tr, "ws://api.example/instances/id/cdp")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/json/version", strings.NewReader("private body")))
	if w.Code != 400 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestCDPConfigurationAndTargetPaths(t *testing.T) {
	for _, base := range []string{"http://api.example/cdp", "ws://user@api.example/cdp", "ws://api.example/cdp?token=secret", "ws://api.example/cdp?", "ws://api.example/cdp#fragment", "ws://api.example/cdp/", "ws:///cdp"} {
		if _, err := NewCDPProxy(http.DefaultTransport, base); err == nil {
			t.Fatalf("accepted base %s", base)
		}
	}
	if _, err := NewCDPProxy(nil, "ws://api.example/cdp"); err == nil {
		t.Fatal("nil transport accepted")
	}
	for _, path := range []string{"/devtools/browser/abc-123", "/devtools/page/ABC123"} {
		if !debuggerPath(path) {
			t.Fatalf("valid path rejected: %s", path)
		}
	}
	for _, path := range []string{"/devtools/browser/", "/devtools/browser/..", "/devtools/page/id/extra", "/devtools/worker/id", "/devtools/page/id.token"} {
		if debuggerPath(path) {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
}
