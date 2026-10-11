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

	"github.com/kernel/hypeman/lib/hypervisor"
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

func TestPauseWaitsForRawPausedStateBeforeSnapshot(t *testing.T) {
	var reads atomic.Int32
	fixture := newUnixHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vmm.ping":
			_, _ = w.Write([]byte("OK"))
		case "/api/v1/vm.pause":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/vm.info":
			state := "Pausing"
			if reads.Add(1) >= 3 {
				state = "Paused"
			}
			_, _ = w.Write([]byte(`{"state":"` + state + `"}`))
		case "/api/v1/vm.snapshot":
			if reads.Load() < 3 {
				http.Error(w, "not paused", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	client, err := NewClient(fixture.socketPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Snapshot(context.Background(), "/snapshot"); err != nil {
		t.Fatal(err)
	}
}

func TestPauseWaitHonorsCancellation(t *testing.T) {
	fixture := newUnixHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vmm.ping":
			_, _ = w.Write([]byte("OK"))
		case "/api/v1/vm.pause":
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"state":"Pausing"}`))
		}
	}))
	client, err := NewClient(fixture.socketPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.Pause(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestValidateSnapshotSupportUsesConfigurationProbe(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
		wantErr  bool
	}{
		{"supported", `{"supported":true}`, false},
		{"unsupported", `{"supported":false,"reason":"unsupported device"}`, true},
		{"missing-result", `{}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newUnixHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/vmm.ping" {
					_, _ = w.Write([]byte("OK"))
					return
				}
				_, _ = w.Write([]byte(test.response))
			}))
			client, err := NewClient(fixture.socketPath())
			if err != nil {
				t.Fatal(err)
			}
			err = client.ValidateSnapshotSupport(context.Background())
			if test.wantErr && !errors.Is(err, hypervisor.ErrNotSupported) {
				t.Fatalf("expected unsupported configuration, got %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
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
