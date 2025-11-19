package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/onkernel/hypeman/lib/exec"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all origins for now - can be tightened in production
		return true
	},
}

// ExecHandler handles exec requests via WebSocket for bidirectional streaming
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

	// Parse request from query parameters (before WebSocket upgrade)
	command := r.URL.Query()["command"]
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}

	tty := r.URL.Query().Get("tty") != "false"

	log.InfoContext(ctx, "exec session started", "id", instanceID, "command", command, "tty", tty)

	// Upgrade to WebSocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.ErrorContext(ctx, "websocket upgrade failed", "error", err)
		return
	}
	defer ws.Close()

	// Create WebSocket read/writer wrapper
	wsConn := &wsReadWriter{ws: ws, ctx: ctx}

	// Execute via vsock
	exit, err := exec.ExecIntoInstance(ctx, inst.VsockSocket, exec.ExecOptions{
		Command: command,
		Stdin:   wsConn,
		Stdout:  wsConn,
		Stderr:  wsConn,
		TTY:     tty,
	})

	if err != nil {
		log.ErrorContext(ctx, "exec failed", "error", err, "id", instanceID)
		// Send error message over WebSocket before closing
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error: %v", err)))
		return
	}

	log.InfoContext(ctx, "exec session ended", "id", instanceID, "exit_code", exit.Code)
}

// wsReadWriter wraps a WebSocket connection to implement io.ReadWriter
type wsReadWriter struct {
	ws     *websocket.Conn
	ctx    context.Context
	reader io.Reader
	mu     sync.Mutex
}

func (w *wsReadWriter) Read(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// If we have a pending reader, continue reading from it
	if w.reader != nil {
		n, err = w.reader.Read(p)
		if err != io.EOF {
			return n, err
		}
		// EOF means we finished this message, get next one
		w.reader = nil
	}

	// Read next WebSocket message
	messageType, data, err := w.ws.ReadMessage()
	if err != nil {
		return 0, err
	}

	// Only handle binary and text messages
	if messageType != websocket.BinaryMessage && messageType != websocket.TextMessage {
		return 0, fmt.Errorf("unexpected message type: %d", messageType)
	}

	// Create reader for this message
	w.reader = bytes.NewReader(data)
	return w.reader.Read(p)
}

func (w *wsReadWriter) Write(p []byte) (n int, err error) {
	if err := w.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

