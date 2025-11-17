package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/system"
)

// ExecHandler handles exec requests via HTTP hijacking for bidirectional streaming
func (s *ApiService) ExecHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logger.FromContext(ctx)

	instanceID := chi.URLParam(r, "id")

	// Get instance
	inst, err := s.InstanceManager.GetInstance(ctx, instanceID)
	if err != nil {
		if err == instances.ErrNotFound {
			http.Error(w, `{"code":"not_found","message":"instance not found"}`, http.StatusNotFound)
			return
		}
		log.ErrorContext(ctx, "failed to get instance", "error", err)
		http.Error(w, `{"code":"internal_error","message":"failed to get instance"}`, http.StatusInternalServerError)
		return
	}

	if inst.State != instances.StateRunning {
		http.Error(w, fmt.Sprintf(`{"code":"invalid_state","message":"instance must be running (current state: %s)"}`, inst.State), http.StatusConflict)
		return
	}

	// Parse request
	var req oapi.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"code":"bad_request","message":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if len(req.Command) == 0 {
		req.Command = []string{"/bin/sh"}
	}

	tty := true
	if req.Tty != nil {
		tty = *req.Tty
	}

	log.InfoContext(ctx, "exec session started", "id", instanceID, "command", req.Command, "tty", tty)

	// Hijack connection for bidirectional streaming
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, `{"code":"internal_error","message":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		log.ErrorContext(ctx, "hijack failed", "error", err)
		return
	}
	defer conn.Close()

	// Send 101 Switching Protocols
	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Upgrade: exec-protocol\r\n\r\n")
	bufrw.Flush()

	// Execute via vsock
	exit, err := system.ExecIntoInstance(ctx, uint32(inst.VsockCID), system.ExecOptions{
		Command: req.Command,
		Stdin:   conn,
		Stdout:  conn,
		Stderr:  conn, // Combined in TTY mode
		TTY:     tty,
	})

	if err != nil {
		log.ErrorContext(ctx, "exec failed", "error", err, "id", instanceID)
		return
	}

	log.InfoContext(ctx, "exec session ended", "id", instanceID, "exit_code", exit.Code)
}

