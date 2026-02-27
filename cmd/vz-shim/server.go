//go:build darwin

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// ShimServer handles control API and vsock proxy for a vz VM.
type ShimServer struct {
	vm         *vz.VirtualMachine
	vmConfig   *vz.VirtualMachineConfiguration
	shimConfig shimconfig.ShimConfig
	mu         sync.RWMutex
}

// NewShimServer creates a new shim server.
func NewShimServer(vm *vz.VirtualMachine, vmConfig *vz.VirtualMachineConfiguration, config shimconfig.ShimConfig) *ShimServer {
	return &ShimServer{
		vm:         vm,
		vmConfig:   vmConfig,
		shimConfig: config,
	}
}

// VMInfoResponse matches the cloud-hypervisor VmInfo structure.
type VMInfoResponse struct {
	State string `json:"state"`
}

type snapshotRequest struct {
	DestinationPath string `json:"destination_path"`
}

// Handler returns the HTTP handler for the control API.
func (s *ShimServer) Handler() http.Handler {
	mux := http.NewServeMux()

	// Match cloud-hypervisor API patterns
	mux.HandleFunc("GET /api/v1/vm.info", s.handleVMInfo)
	mux.HandleFunc("PUT /api/v1/vm.pause", s.handlePause)
	mux.HandleFunc("PUT /api/v1/vm.resume", s.handleResume)
	mux.HandleFunc("PUT /api/v1/vm.shutdown", s.handleShutdown)
	mux.HandleFunc("PUT /api/v1/vm.snapshot", s.handleSnapshot)
	mux.HandleFunc("PUT /api/v1/vm.power-button", s.handlePowerButton)
	mux.HandleFunc("GET /api/v1/vmm.ping", s.handlePing)
	mux.HandleFunc("PUT /api/v1/vmm.shutdown", s.handleVMMShutdown)

	return mux
}

func (s *ShimServer) handleVMInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state := vzStateToString(s.vm.State())
	resp := VMInfoResponse{State: state}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *ShimServer) handlePause(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.vm.CanPause() {
		http.Error(w, "cannot pause VM", http.StatusBadRequest)
		return
	}

	if err := s.vm.Pause(); err != nil {
		slog.Error("failed to pause VM", "error", err)
		http.Error(w, fmt.Sprintf("pause failed: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("VM paused")
	w.WriteHeader(http.StatusNoContent)
}

func (s *ShimServer) handleResume(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.vm.CanResume() {
		http.Error(w, "cannot resume VM", http.StatusBadRequest)
		return
	}

	if err := s.vm.Resume(); err != nil {
		slog.Error("failed to resume VM", "error", err)
		http.Error(w, fmt.Sprintf("resume failed: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("VM resumed")
	w.WriteHeader(http.StatusNoContent)
}

func (s *ShimServer) handleShutdown(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Send ACPI power button (graceful shutdown signal to guest).
	// The caller (instance manager) handles timeout/force-kill if the guest
	// doesn't shut down in time. Force-kill is in handleVMMShutdown / killHypervisor.
	success, err := s.vm.RequestStop()
	if err != nil || !success {
		slog.Error("RequestStop failed", "error", err, "success", success)
		http.Error(w, fmt.Sprintf("shutdown failed: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("VM graceful shutdown requested (ACPI power button)")
	w.WriteHeader(http.StatusNoContent)
}

func (s *ShimServer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var req snapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid snapshot request: %v", err), http.StatusBadRequest)
		return
	}
	if req.DestinationPath == "" {
		http.Error(w, "destination_path is required", http.StatusBadRequest)
		return
	}
	if s.vm.State() != vz.VirtualMachineStatePaused {
		http.Error(w, "vm must be paused before snapshot", http.StatusBadRequest)
		return
	}
	if err := validateSaveRestoreSupport(s.vmConfig); err != nil {
		http.Error(w, fmt.Sprintf("save/restore not supported: %v", err), http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(req.DestinationPath, 0755); err != nil {
		http.Error(w, fmt.Sprintf("create snapshot dir failed: %v", err), http.StatusInternalServerError)
		return
	}
	snapshotComplete := false
	defer func() {
		if !snapshotComplete {
			_ = os.RemoveAll(req.DestinationPath)
		}
	}()

	machineStatePath := filepath.Join(req.DestinationPath, shimconfig.SnapshotMachineStateFile)
	if err := os.RemoveAll(machineStatePath); err != nil {
		http.Error(w, fmt.Sprintf("prepare machine state path failed: %v", err), http.StatusInternalServerError)
		return
	}
	if err := saveMachineState(s.vm, machineStatePath); err != nil {
		http.Error(w, fmt.Sprintf("save machine state failed: %v", err), http.StatusInternalServerError)
		return
	}

	manifestPath := filepath.Join(req.DestinationPath, shimconfig.SnapshotManifestFile)
	tmpManifestPath := manifestPath + ".tmp"
	manifest := shimconfig.SnapshotManifest{
		Hypervisor:       "vz",
		MachineStateFile: shimconfig.SnapshotMachineStateFile,
		ShimConfig:       s.shimConfig,
	}
	// This field is runtime-only; restore path is populated by the caller on restore.
	manifest.ShimConfig.RestoreMachineStatePath = ""
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		http.Error(w, fmt.Sprintf("marshal manifest failed: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(tmpManifestPath, manifestBytes, 0644); err != nil {
		http.Error(w, fmt.Sprintf("write manifest failed: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpManifestPath, manifestPath); err != nil {
		http.Error(w, fmt.Sprintf("finalize manifest failed: %v", err), http.StatusInternalServerError)
		return
	}

	snapshotComplete = true
	slog.Info("VM snapshot saved", "destination", req.DestinationPath, "machine_state", machineStatePath)
	w.WriteHeader(http.StatusNoContent)
}

func (s *ShimServer) handlePowerButton(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// RequestStop sends an ACPI power button event
	success, err := s.vm.RequestStop()
	if err != nil || !success {
		slog.Error("failed to send power button", "error", err, "success", success)
		http.Error(w, fmt.Sprintf("power button failed: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("power button sent")
	w.WriteHeader(http.StatusNoContent)
}

func (s *ShimServer) handlePing(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *ShimServer) handleVMMShutdown(w http.ResponseWriter, r *http.Request) {
	slog.Info("VMM shutdown requested")
	w.WriteHeader(http.StatusNoContent)

	// Stop the VM and exit
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.vm.CanStop() {
			s.vm.Stop()
		}
		// Process will exit when VM stops (monitored in main)
	}()
}

func vzStateToString(state vz.VirtualMachineState) string {
	switch state {
	case vz.VirtualMachineStateStopped:
		return "Shutdown"
	case vz.VirtualMachineStateRunning:
		return "Running"
	case vz.VirtualMachineStatePaused:
		return "Paused"
	case vz.VirtualMachineStateError:
		return "Error"
	case vz.VirtualMachineStateStarting:
		return "Starting"
	case vz.VirtualMachineStatePausing:
		return "Pausing"
	case vz.VirtualMachineStateResuming:
		return "Resuming"
	case vz.VirtualMachineStateStopping:
		return "Stopping"
	case vz.VirtualMachineStateSaving:
		return "Saving"
	case vz.VirtualMachineStateRestoring:
		return "Restoring"
	default:
		return "Unknown"
	}
}

// ServeVsock handles vsock proxy connections using the Cloud Hypervisor protocol.
// Protocol: Client sends "CONNECT {port}\n", server responds "OK {port}\n", then proxies.
func (s *ShimServer) ServeVsock(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			slog.Debug("vsock listener closed", "error", err)
			return
		}
		go s.handleVsockConnection(conn)
	}
}

func (s *ShimServer) handleVsockConnection(conn net.Conn) {
	defer conn.Close()

	// Read the CONNECT command
	reader := bufio.NewReader(conn)
	cmd, err := reader.ReadString('\n')
	if err != nil {
		slog.Error("failed to read vsock handshake", "error", err)
		return
	}

	// Parse "CONNECT {port}\n"
	var port uint32
	if _, err := fmt.Sscanf(cmd, "CONNECT %d\n", &port); err != nil {
		slog.Error("invalid vsock handshake", "cmd", cmd, "error", err)
		conn.Write([]byte(fmt.Sprintf("ERR invalid command: %s", cmd)))
		return
	}

	slog.Debug("vsock connect request", "port", port)

	// Get vsock device and connect to guest
	s.mu.RLock()
	socketDevices := s.vm.SocketDevices()
	s.mu.RUnlock()

	if len(socketDevices) == 0 {
		slog.Error("no vsock device configured")
		conn.Write([]byte("ERR no vsock device\n"))
		return
	}

	guestConn, err := socketDevices[0].Connect(port)
	if err != nil {
		slog.Error("failed to connect to guest vsock", "port", port, "error", err)
		conn.Write([]byte(fmt.Sprintf("ERR connect failed: %v\n", err)))
		return
	}
	defer guestConn.Close()

	// Send OK response (matching CH protocol)
	if _, err := conn.Write([]byte(fmt.Sprintf("OK %d\n", port))); err != nil {
		slog.Error("failed to send OK response", "error", err)
		return
	}

	slog.Debug("vsock connection established", "port", port)

	// Proxy data bidirectionally
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(guestConn, reader)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn, guestConn)
		done <- struct{}{}
	}()

	// Wait for one direction to close
	<-done
}
