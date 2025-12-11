package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/onkernel/hypeman/lib/exec"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	mw "github.com/onkernel/hypeman/lib/middleware"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all origins for now - can be tightened in production
		return true
	},
}

// ExecRequest represents the JSON body for exec requests
type ExecRequest struct {
	Command []string          `json:"command"`
	TTY     bool              `json:"tty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout int32             `json:"timeout,omitempty"` // seconds
}

// ExecHandler handles exec requests via WebSocket for bidirectional streaming
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) ExecHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	startTime := time.Now()
	log := logger.FromContext(ctx)

	// Get instance resolved by middleware
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		http.Error(w, `{"code":"internal_error","message":"resource not resolved"}`, http.StatusInternalServerError)
		return
	}

	if inst.State != instances.StateRunning {
		http.Error(w, fmt.Sprintf(`{"code":"invalid_state","message":"instance must be running (current state: %s)"}`, inst.State), http.StatusConflict)
		return
	}

	// Upgrade to WebSocket first
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.ErrorContext(ctx, "websocket upgrade failed", "error", err)
		return
	}
	defer ws.Close()

	// Read JSON request from first WebSocket message
	msgType, message, err := ws.ReadMessage()
	if err != nil {
		log.ErrorContext(ctx, "failed to read exec request", "error", err)
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"error":"failed to read request: %v"}`, err)))
		return
	}

	if msgType != websocket.TextMessage {
		log.ErrorContext(ctx, "expected text message with JSON request", "type", msgType)
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"first message must be JSON text"}`))
		return
	}

	// Parse JSON request
	var execReq ExecRequest
	if err := json.Unmarshal(message, &execReq); err != nil {
		log.ErrorContext(ctx, "invalid JSON request", "error", err)
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"error":"invalid JSON: %v"}`, err)))
		return
	}

	// Default command if not specified
	if len(execReq.Command) == 0 {
		execReq.Command = []string{"/bin/sh"}
	}

	// Get JWT subject for audit logging (if available)
	subject := "unknown"
	if claims, ok := r.Context().Value("claims").(map[string]interface{}); ok {
		if sub, ok := claims["sub"].(string); ok {
			subject = sub
		}
	}

	// Audit log: exec session started
	log.InfoContext(ctx, "exec session started",
		"instance_id", inst.Id,
		"subject", subject,
		"command", execReq.Command,
		"tty", execReq.TTY,
		"cwd", execReq.Cwd,
		"timeout", execReq.Timeout,
	)

	// Create WebSocket read/writer wrapper
	wsConn := &wsReadWriter{ws: ws, ctx: ctx}

	// Execute via vsock
	exit, err := exec.ExecIntoInstance(ctx, inst.VsockSocket, exec.ExecOptions{
		Command: execReq.Command,
		Stdin:   wsConn,
		Stdout:  wsConn,
		Stderr:  wsConn,
		TTY:     execReq.TTY,
		Env:     execReq.Env,
		Cwd:     execReq.Cwd,
		Timeout: execReq.Timeout,
	})

	duration := time.Since(startTime)

	if err != nil {
		log.ErrorContext(ctx, "exec failed",
			"error", err,
			"instance_id", inst.Id,
			"subject", subject,
			"duration_ms", duration.Milliseconds(),
		)
		// Send error message over WebSocket before closing
		// Use BinaryMessage so the CLI writes it to stdout (it ignores TextMessage for output)
		// Use \r\n so it displays properly when client terminal is in raw mode
		ws.WriteMessage(websocket.BinaryMessage, []byte(fmt.Sprintf("Error: %v\r\n", err)))
		// Send exit code 127 (command not found - standard Unix convention)
		ws.WriteMessage(websocket.TextMessage, []byte(`{"exitCode":127}`))
		return
	}

	// Audit log: exec session ended
	log.InfoContext(ctx, "exec session ended",
		"instance_id", inst.Id,
		"subject", subject,
		"exit_code", exit.Code,
		"duration_ms", duration.Milliseconds(),
	)

	// Send close frame with exit code in JSON
	closeMsg := fmt.Sprintf(`{"exitCode":%d}`, exit.Code)
	ws.WriteMessage(websocket.TextMessage, []byte(closeMsg))
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
