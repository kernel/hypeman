//go:build darwin

// Package main implements hypeman-vz-shim, a subprocess that hosts vz VMs.
// This allows VMs to survive hypeman restarts by running in a separate process.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

func main() {
	configJSON := flag.String("config", "", "VM configuration as JSON")
	flag.Parse()

	if *configJSON == "" {
		fmt.Fprintln(os.Stderr, "error: -config is required")
		os.Exit(1)
	}

	var config shimconfig.ShimConfig
	if err := json.Unmarshal([]byte(*configJSON), &config); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid config JSON: %v\n", err)
		os.Exit(1)
	}

	// Setup logging to file
	if err := setupLogging(config.LogPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: setup logging: %v\n", err)
		os.Exit(1)
	}

	slog.Info("vz-shim starting", "control_socket", config.ControlSocket, "vsock_socket", config.VsockSocket)

	// Create the VM
	vm, vmConfig, err := createVM(&config)
	if err != nil {
		slog.Error("failed to create VM", "error", err)
		fmt.Fprintf(os.Stderr, "failed to create VM: %v\n", err)
		os.Exit(1)
	}

	if config.RestoreMachineStatePath != "" {
		if err := validateSaveRestoreSupport(vmConfig); err != nil {
			slog.Error("save/restore not supported for VM config", "error", err)
			fmt.Fprintf(os.Stderr, "save/restore not supported for VM config: %v\n", err)
			os.Exit(1)
		}
		if err := restoreMachineState(vm, config.RestoreMachineStatePath); err != nil {
			slog.Error("failed to restore VM machine state", "error", err, "path", config.RestoreMachineStatePath)
			fmt.Fprintf(os.Stderr, "failed to restore VM machine state: %v\n", err)
			os.Exit(1)
		}
		slog.Info("VM restored from machine state", "path", config.RestoreMachineStatePath, "state", vm.State())
	} else {
		if err := vm.Start(); err != nil {
			slog.Error("failed to start VM", "error", err)
			fmt.Fprintf(os.Stderr, "failed to start VM: %v\n", err)
			os.Exit(1)
		}
		slog.Info("VM started", "vcpus", config.VCPUs, "memory_mb", config.MemoryBytes/1024/1024)
	}

	// Create the shim server
	server := NewShimServer(vm, vmConfig, config)

	// Start control socket listener (remove stale socket from previous run)
	os.Remove(config.ControlSocket)
	controlListener, err := net.Listen("unix", config.ControlSocket)
	if err != nil {
		slog.Error("failed to listen on control socket", "error", err, "path", config.ControlSocket)
		os.Exit(1)
	}
	defer controlListener.Close()

	// Start vsock proxy listener (remove stale socket from previous run)
	os.Remove(config.VsockSocket)
	vsockListener, err := net.Listen("unix", config.VsockSocket)
	if err != nil {
		slog.Error("failed to listen on vsock socket", "error", err, "path", config.VsockSocket)
		os.Exit(1)
	}
	defer vsockListener.Close()

	// Start HTTP server for control API
	httpServer := &http.Server{Handler: server.Handler()}
	go func() {
		slog.Info("control API listening", "socket", config.ControlSocket)
		if err := httpServer.Serve(controlListener); err != nil && err != http.ErrServerClosed {
			slog.Error("control API server error", "error", err)
		}
	}()

	// Start vsock proxy
	go func() {
		slog.Info("vsock proxy listening", "socket", config.VsockSocket)
		server.ServeVsock(vsockListener)
	}()

	// Wait for shutdown signal or VM stop
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// Monitor VM state
	stateChanged := vm.StateChangedNotify()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case newState := <-stateChanged:
				slog.Info("VM state changed", "state", newState)
				if newState == vz.VirtualMachineStateStopped || newState == vz.VirtualMachineStateError {
					slog.Info("VM stopped, shutting down shim")
					cancel()
					return
				}
			}
		}
	}()

	select {
	case sig := <-sigChan:
		slog.Info("received signal, shutting down", "signal", sig)
	case <-ctx.Done():
		slog.Info("context cancelled, shutting down")
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	httpServer.Shutdown(shutdownCtx)

	if vm.State() == vz.VirtualMachineStateRunning {
		slog.Info("stopping VM")
		if vm.CanStop() {
			vm.Stop()
		}
	}

	slog.Info("vz-shim shutdown complete")
}

func setupLogging(logPath string) error {
	if logPath == "" {
		// Log to stderr if no path specified
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return nil
}
