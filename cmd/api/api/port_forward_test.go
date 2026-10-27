package api

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/guest/portforward"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/stretchr/testify/require"
)

// forwardingDialer stands in for the guest agent: it answers the port forward
// handshake and connects every forward to backend, whatever port is named.
type forwardingDialer struct {
	backend  string
	ports    chan uint16
	dialErr  error // the forwarder itself is unreachable
	unopened bool  // nothing listens on the guest port
}

func (d *forwardingDialer) DialVsock(_ context.Context, port int) (net.Conn, error) {
	if port != portforward.VsockPort {
		return nil, io.ErrUnexpectedEOF
	}
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	host, agent := net.Pipe()
	go func() {
		defer agent.Close()
		var target uint16
		if err := binary.Read(agent, binary.BigEndian, &target); err != nil {
			return
		}
		d.ports <- target
		if d.unopened {
			_, _ = agent.Write([]byte{portforward.StatusDialFailed})
			return
		}
		conn, err := net.Dial("tcp", d.backend)
		if err != nil {
			_, _ = agent.Write([]byte{portforward.StatusDialFailed})
			return
		}
		defer conn.Close()
		_, _ = agent.Write([]byte{portforward.StatusOK})
		go func() { _, _ = io.Copy(conn, agent) }()
		_, _ = io.Copy(agent, conn)
	}()
	return host, nil
}

func (d *forwardingDialer) Key() string { return "forwarding" }

type portForwardManager struct {
	instances.Manager
	dialer    hypervisor.VsockDialer
	dialerErr error
}

func (m portForwardManager) GetVsockDialer(context.Context, string) (hypervisor.VsockDialer, error) {
	return m.dialer, m.dialerErr
}

func newPortForwardServer(t *testing.T, inst *instances.Instance, backend http.Handler, configure ...func(*forwardingDialer, *portForwardManager)) (*httptest.Server, chan uint16) {
	t.Helper()
	upstream := httptest.NewServer(backend)
	t.Cleanup(upstream.Close)
	dialer := &forwardingDialer{backend: strings.TrimPrefix(upstream.URL, "http://"), ports: make(chan uint16, 8)}
	manager := portForwardManager{dialer: dialer}
	for _, c := range configure {
		c(dialer, &manager)
	}
	s := &ApiService{InstanceManager: manager}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(mw.WithResolvedInstance(r.Context(), inst.Id, inst)))
		})
	})
	for _, method := range scopes.PortForwardMethods {
		r.MethodFunc(method, scopes.PortForwardRoute, s.PortForwardHandler)
	}
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return server, dialer.ports
}

func runningMacOSInstance() *instances.Instance {
	return &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "mac", MacOS: &images.MacOSImage{GuestAgent: true}, GuestAgent: &instances.GuestAgentInfo{
			ProtocolVersion: 1,
			Capabilities:    []string{guest.AgentCapability_AGENT_CAPABILITY_PORT_FORWARD.String()},
		}},
		State: instances.StateRunning,
	}
}

func TestPortForwardForwardsHTTP(t *testing.T) {
	var gotPath, gotQuery, gotHost, gotAuth string
	server, ports := newPortForwardServer(t, runningMacOSInstance(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotHost, gotAuth = r.URL.Path, r.URL.RawQuery, r.Host, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"Browser":"Chrome"}`)
	}))

	req, err := http.NewRequest(http.MethodGet, server.URL+"/instances/mac/ports/9222/json/version?x=1", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer hypeman-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, `{"Browser":"Chrome"}`, string(body))
	require.EqualValues(t, 9222, <-ports)
	require.Equal(t, "/json/version", gotPath)
	require.Equal(t, "x=1", gotQuery)
	require.Equal(t, "127.0.0.1:9222", gotHost, "guest services see a loopback Host")
	require.Empty(t, gotAuth, "hypeman credentials must not reach the guest")
}

func TestPortForwardForwardsWebSocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server, _ := newPortForwardServer(t, runningMacOSInstance(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(kind, data)
	}))

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/instances/mac/ports/9222/devtools/browser/abc", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"id":1}`)))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, `{"id":1}`, string(data))
}

func TestPortForwardRejections(t *testing.T) {
	noAgent := runningMacOSInstance()
	noAgent.MacOS.GuestAgent = false
	noCapability := runningMacOSInstance()
	noCapability.GuestAgent.Capabilities = nil
	stopped := runningMacOSInstance()
	stopped.State = instances.StateStopped
	linux := &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "linux"}, State: instances.StateRunning}

	for _, tc := range []struct {
		name string
		inst *instances.Instance
		port string
		code int
	}{
		{"linux guest", linux, "9222", http.StatusNotImplemented},
		{"macOS without agent", noAgent, "9222", http.StatusNotImplemented},
		{"agent without the capability", noCapability, "9222", http.StatusNotImplemented},
		{"port zero", runningMacOSInstance(), "0", http.StatusBadRequest},
		{"port out of range", runningMacOSInstance(), "70000", http.StatusBadRequest},
		{"stopped", stopped, "9222", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newPortForwardServer(t, tc.inst, http.NotFoundHandler())
			resp, err := http.Get(server.URL + "/instances/" + tc.inst.Id + "/ports/" + tc.port + "/json")
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, tc.code, resp.StatusCode)
		})
	}
}

func TestPortForwardKeepsEscapedPath(t *testing.T) {
	var gotRawPath string
	server, _ := newPortForwardServer(t, runningMacOSInstance(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
	}))
	resp, err := http.Get(server.URL + "/instances/mac/ports/9222/a%2Fb/c%20d")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/a%2Fb/c%20d", gotRawPath, "escapes reach the guest once, not twice")
}

func TestPortForwardForwardFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*forwardingDialer, *portForwardManager)
		code      int
		body      string
	}{
		{"nothing listening", func(d *forwardingDialer, _ *portForwardManager) { d.unopened = true }, http.StatusBadGateway, "port_unavailable"},
		{"forwarder unreachable", func(d *forwardingDialer, _ *portForwardManager) { d.dialErr = errors.New("connection refused") }, http.StatusServiceUnavailable, "unavailable"},
		{"agent disabled", func(_ *forwardingDialer, m *portForwardManager) { m.dialerErr = instances.ErrInvalidRequest }, http.StatusServiceUnavailable, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newPortForwardServer(t, runningMacOSInstance(), http.NotFoundHandler(), tc.configure)
			resp, err := http.Get(server.URL + "/instances/mac/ports/9222/json")
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, tc.code, resp.StatusCode)
			require.Contains(t, string(body), tc.body)
		})
	}
}
