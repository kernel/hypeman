package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/desktop"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/stretchr/testify/require"
)

func TestDesktopWebsocketReconnect(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("guest received API authority")
			http.Error(w, "unexpected headers", 400)
			return
		}
		if r.URL.Path == "/status" {
			_ = json.NewEncoder(w).Encode(desktop.Status{Version: 1, OS: "darwin", Architecture: "arm64", UID: 501, ConsoleUID: 501, GUISession: true, BrowserManaged: true, BrowserReady: true})
			return
		}
		if r.URL.Path != "/devtools/browser/test-id" {
			t.Error("unexpected browser route")
			http.Error(w, "bad route", 400)
			return
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteMessage(kind, data); err != nil {
			t.Error(err)
		}
	}))
	defer guest.Close()
	s := &ApiService{Config: &config.Config{MacOSDesktopOrigin: "https://api.example"}, InstanceManager: &desktopInstances{inst: desktopInstance()}}
	factory := func(*instances.Instance) (*http.Transport, error) {
		return &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(guest.URL, "http://"))
		}}, nil
	}
	api := httptest.NewServer(desktopRouter(s, factory))
	defer api.Close()
	for i := 0; i < 2; i++ {
		headers := http.Header{"Authorization": {"Bearer " + desktopToken(t, string(scopes.InstanceWrite))}, "Cookie": {"secret=value"}, "Origin": {"https://api.example"}}
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(api.URL, "http")+"/instances/test/cdp/devtools/browser/test-id", headers)
		require.NoError(t, err)
		require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"method":"Browser.getVersion"}`)))
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		require.Contains(t, string(data), "Browser.getVersion")
		require.NoError(t, c.Close())
		require.Eventually(t, func() bool { return len(s.desktopSlots) == 0 }, 2*time.Second, 10*time.Millisecond)
	}
}

func TestDesktopActiveSessionLimit(t *testing.T) {
	s := &ApiService{Config: &config.Config{MacOSDesktopOrigin: "https://api.example"}, InstanceManager: &desktopInstances{inst: desktopInstance()}}
	s.desktopSlotsOnce.Do(func() { s.desktopSlots = make(chan struct{}, desktop.MaxSessions) })
	for i := 0; i < desktop.MaxSessions; i++ {
		s.desktopSlots <- struct{}{}
	}
	r := httptest.NewRequest("GET", "http://api.example/instances/test/cdp/status", nil)
	r.Header.Set("Authorization", "Bearer "+desktopToken(t, string(scopes.InstanceWrite)))
	w := httptest.NewRecorder()
	desktopRouter(s, func(*instances.Instance) (*http.Transport, error) {
		t.Fatal("dial above session limit")
		return nil, nil
	}).ServeHTTP(w, r)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Len(t, s.desktopSlots, desktop.MaxSessions)
}
