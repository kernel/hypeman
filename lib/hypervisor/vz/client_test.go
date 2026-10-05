//go:build darwin

package vz

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const connectionCloseTimeout = 2 * time.Second

type unixHTTPFixture struct {
	listener net.Listener
	server   *http.Server

	mu    sync.Mutex
	conns map[net.Conn]http.ConnState
}

func newUnixHTTPFixture(t *testing.T, handler http.Handler) *unixHTTPFixture {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "hypeman-vz-test-")
	if err != nil {
		t.Fatalf("create temporary Unix socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	listener, err := net.Listen("unix", filepath.Join(dir, "vz.sock"))
	if err != nil {
		t.Fatalf("listen on temporary Unix socket: %v", err)
	}
	fixture := &unixHTTPFixture{
		listener: listener,
		conns:    make(map[net.Conn]http.ConnState),
	}
	fixture.server = &http.Server{
		Handler: handler,
		ConnState: func(conn net.Conn, state http.ConnState) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if state == http.StateClosed || state == http.StateHijacked {
				delete(fixture.conns, conn)
				return
			}
			fixture.conns[conn] = state
		},
	}
	go func() {
		_ = fixture.server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = fixture.server.Close()
	})
	return fixture
}

func (f *unixHTTPFixture) socketPath() string {
	return f.listener.Addr().String()
}

func (f *unixHTTPFixture) waitForNoConnections(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(connectionCloseTimeout)
	for {
		f.mu.Lock()
		count := len(f.conns)
		f.mu.Unlock()
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary VZ server retained %d connection(s)", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRepeatedClientsReleaseSuccessfulControlConnections(t *testing.T) {
	fixture := newUnixHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vmm.ping":
			_, _ = w.Write([]byte("OK"))
		case "/api/v1/vm.info":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"Running"}`))
		default:
			http.NotFound(w, r)
		}
	}))

	for range 128 {
		client, err := NewClient(fixture.socketPath())
		if err != nil {
			t.Fatalf("create VZ client: %v", err)
		}
		if _, err := client.GetVMInfo(context.Background()); err != nil {
			t.Fatalf("get VM info: %v", err)
		}
		fixture.waitForNoConnections(t)
	}
}

func TestControlConnectionsCloseAfterResponseErrors(t *testing.T) {
	t.Run("http error", func(t *testing.T) {
		fixture := newUnixHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/vmm.ping" {
				_, _ = w.Write([]byte("OK"))
				return
			}
			http.Error(w, "pause failed", http.StatusInternalServerError)
		}))

		client, err := NewClient(fixture.socketPath())
		if err != nil {
			t.Fatalf("create VZ client: %v", err)
		}
		if err := client.Pause(context.Background()); err == nil {
			t.Fatal("expected HTTP error")
		}
		fixture.waitForNoConnections(t)
	})

	t.Run("invalid json", func(t *testing.T) {
		fixture := newUnixHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/vmm.ping" {
				_, _ = w.Write([]byte("OK"))
				return
			}
			_, _ = w.Write([]byte("not-json"))
		}))

		client, err := NewClient(fixture.socketPath())
		if err != nil {
			t.Fatalf("create VZ client: %v", err)
		}
		if _, err := client.GetVMInfo(context.Background()); err == nil {
			t.Fatal("expected JSON decoding error")
		}
		fixture.waitForNoConnections(t)
	})
}

func TestControlConnectionClosesAfterCancellation(t *testing.T) {
	requestStarted := make(chan struct{})
	var startedOnce sync.Once
	var sawCancellation atomic.Bool
	fixture := newUnixHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vmm.ping" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		startedOnce.Do(func() { close(requestStarted) })
		<-r.Context().Done()
		sawCancellation.Store(true)
	}))

	client, err := NewClient(fixture.socketPath())
	if err != nil {
		t.Fatalf("create VZ client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.GetVMInfo(ctx)
		result <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("VM info request did not reach the server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("VM info request did not return after cancellation")
	}
	fixture.waitForNoConnections(t)
	deadline := time.Now().Add(time.Second)
	for !sawCancellation.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !sawCancellation.Load() {
		t.Fatal("server did not observe request cancellation")
	}
}
