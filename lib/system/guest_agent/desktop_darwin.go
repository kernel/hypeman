//go:build darwin

package main

import (
	"net/http"
	"os"
	"time"

	"github.com/kernel/hypeman/lib/desktop"
)

func runDesktopAgent() error {
	backend, err := desktop.NewDarwinBackend()
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	proxy, err := desktop.NewCDPProxy(transport, "ws://127.0.0.1:9222")
	if err != nil {
		return err
	}
	service, err := desktop.NewService(backend, uint32(os.Getuid()), proxy)
	if err != nil {
		return err
	}
	// Reuse the native host-CID-only listener, not a guest TCP listener or root RPC.
	listener, err := listenVsock(desktop.AgentPort)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: service, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	return server.Serve(listener)
}
