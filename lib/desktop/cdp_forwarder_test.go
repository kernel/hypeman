package desktop

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCDPForwarderLeavesDiscoveryToHostBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		status        int
	}{
		{"valid", ` {"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/id","devtoolsFrontendUrl":"unsafe"} `, 200},
		{"foreign-host", `{"webSocketDebuggerUrl":"ws://evil.example/devtools/browser/id"}`, 502},
		{"bad-json", `not json`, 502},
		{"oversized", strings.Repeat(" ", maxDiscoveryBytes+1), 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, key := range []string{"Authorization", "Cookie", "Origin", "X-API-Key", "X-Forwarded-For"} {
					if r.Header.Get(key) != "" {
						t.Errorf("forwarded %s", key)
					}
				}
				w.Header().Set("Set-Cookie", "private=yes")
				io.WriteString(w, tc.payload)
			}))
			defer browser.Close()
			forwarder, err := NewCDPForwarder(testTransport(t, browser))
			if err != nil {
				t.Fatal(err)
			}
			// The guest forwards bytes, not a second JSON transform.
			direct := httptest.NewRecorder()
			forwarder.ServeHTTP(direct, httptest.NewRequest("GET", "http://guest/json/version", nil))
			if direct.Code != 200 || direct.Body.String() != tc.payload || direct.Header().Get("Set-Cookie") != "" {
				t.Fatal("guest modified discovery or leaked cookies")
			}
			guest := httptest.NewServer(forwarder)
			defer guest.Close()
			host, err := NewCDPProxy(testTransport(t, guest), "wss://api.example/instances/id/cdp")
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", "http://api.example/json/version", nil)
			request.Header.Set("Authorization", "private")
			request.Header.Set("Cookie", "private=yes")
			response := httptest.NewRecorder()
			host.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("host status=%d want %d", response.Code, tc.status)
			}
			if tc.status == 200 && (!strings.Contains(response.Body.String(), "wss://api.example/instances/id/cdp/devtools/browser/id") || strings.Contains(response.Body.String(), "unsafe")) {
				t.Fatal("host failed discovery transformation")
			}
		})
	}
}

func TestCDPForwarderRejectsRequestsAndRedirects(t *testing.T) {
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://evil.example")
		w.WriteHeader(302)
	}))
	defer browser.Close()
	handler, err := NewCDPForwarder(testTransport(t, browser))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/json/new", 400}, {"POST", "/json/version", 400}, {"GET", "/json/version?url=evil", 400}, {"GET", "/json/version", 502}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, "http://guest"+tc.path, nil))
		if response.Code != tc.status {
			t.Fatalf("%s %s=%d", tc.method, tc.path, response.Code)
		}
	}
}
