package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
)

func TestMetricsServerAddress(t *testing.T) {
	cfg := &config.Config{}
	cfg.Metrics.ListenAddress = "127.0.0.1"
	cfg.Metrics.Port = 9464

	got := metricsServerAddress(cfg)
	want := "127.0.0.1:9464"
	if got != want {
		t.Fatalf("expected metrics address %q, got %q", want, got)
	}
}

func TestMetricsServerServesAndShutsDown(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hypeman_test_metric 1\n"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := newMetricsServer(ln.Addr().String(), h)
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "hypeman_test_metric") {
		t.Fatalf("expected test metric in body")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown server: %v", err)
	}

	serveErr := <-errCh
	if !errors.Is(serveErr, http.ErrServerClosed) {
		t.Fatalf("expected ErrServerClosed after shutdown, got %v", serveErr)
	}
}

func TestMetricsServerBindFailure(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer occupied.Close()

	srv := newMetricsServer(occupied.Addr().String(), h)
	err = srv.ListenAndServe()
	if err == nil {
		t.Fatalf("expected bind failure")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("expected bind error, got ErrServerClosed")
	}
}
