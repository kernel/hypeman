package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/onkernel/hypeman/lib/guest"
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	mw "github.com/onkernel/hypeman/lib/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// cpErrorSent wraps an error that has already been sent to the client.
// The caller should log this error but not send it again to avoid duplicates.
type cpErrorSent struct {
	err error
}

func (e *cpErrorSent) Error() string { return e.err.Error() }
func (e *cpErrorSent) Unwrap() error { return e.err }

// CpRequest represents the JSON body for copy requests
type CpRequest struct {
	// Direction: "to" copies from client to guest, "from" copies from guest to client
	Direction string `json:"direction"`
	// Path in the guest filesystem
	GuestPath string `json:"guest_path"`
	// IsDir indicates if the source is a directory (for "to" direction)
	IsDir bool `json:"is_dir,omitempty"`
	// Mode is the file mode/permissions (for "to" direction, optional)
	Mode uint32 `json:"mode,omitempty"`
	// FollowLinks follows symbolic links (for "from" direction)
	FollowLinks bool `json:"follow_links,omitempty"`
	// SrcBasename is the source file/dir basename (for "to" direction, used for path resolution)
	SrcBasename string `json:"src_basename,omitempty"`
	// Uid is the user ID (archive mode, for "to" direction)
	Uid uint32 `json:"uid,omitempty"`
	// Gid is the group ID (archive mode, for "to" direction)
	Gid uint32 `json:"gid,omitempty"`
}

// CpFileHeader is sent before file data in WebSocket protocol
type CpFileHeader struct {
	Type       string `json:"type"` // "header"
	Path       string `json:"path"`
	Mode       uint32 `json:"mode"`
	IsDir      bool   `json:"is_dir"`
	IsSymlink  bool   `json:"is_symlink,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
	Size       int64  `json:"size"`
	Mtime      int64  `json:"mtime"`
	Uid        uint32 `json:"uid,omitempty"`
	Gid        uint32 `json:"gid,omitempty"`
}

// CpEndMarker signals end of file or transfer
type CpEndMarker struct {
	Type  string `json:"type"` // "end"
	Final bool   `json:"final"`
}

// CpError reports an error
type CpError struct {
	Type    string `json:"type"` // "error"
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

// CpResult reports the result of a copy-to operation
type CpResult struct {
	Type         string `json:"type"` // "result"
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	BytesWritten int64  `json:"bytes_written,omitempty"`
}

// CpHandler handles file copy requests via WebSocket
func (s *ApiService) CpHandler(w http.ResponseWriter, r *http.Request) {
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

	// Upgrade to WebSocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.ErrorContext(ctx, "websocket upgrade failed", "error", err)
		return
	}
	defer ws.Close()

	// Read JSON request from first WebSocket message
	msgType, message, err := ws.ReadMessage()
	if err != nil {
		log.ErrorContext(ctx, "failed to read cp request", "error", err)
		errMsg, _ := json.Marshal(CpError{Type: "error", Message: fmt.Sprintf("failed to read request: %v", err)})
		ws.WriteMessage(websocket.TextMessage, errMsg)
		return
	}

	if msgType != websocket.TextMessage {
		log.ErrorContext(ctx, "expected text message with JSON request", "type", msgType)
		errMsg, _ := json.Marshal(CpError{Type: "error", Message: "first message must be JSON text"})
		ws.WriteMessage(websocket.TextMessage, errMsg)
		return
	}

	// Parse JSON request
	var cpReq CpRequest
	if err := json.Unmarshal(message, &cpReq); err != nil {
		log.ErrorContext(ctx, "invalid JSON request", "error", err)
		errMsg, _ := json.Marshal(CpError{Type: "error", Message: fmt.Sprintf("invalid JSON: %v", err)})
		ws.WriteMessage(websocket.TextMessage, errMsg)
		return
	}

	// Get JWT subject for audit logging
	subject := "unknown"
	if claims, ok := r.Context().Value("claims").(map[string]interface{}); ok {
		if sub, ok := claims["sub"].(string); ok {
			subject = sub
		}
	}

	// Start OTEL span for tracing (WebSocket bypasses otelchi middleware)
	tracer := otel.Tracer("hypeman/cp")
	ctx, span := tracer.Start(ctx, "cp.session",
		trace.WithAttributes(
			attribute.String("instance_id", inst.Id),
			attribute.String("direction", cpReq.Direction),
			attribute.String("guest_path", cpReq.GuestPath),
			attribute.String("subject", subject),
		),
	)
	defer span.End()

	log.InfoContext(ctx, "cp session started",
		"instance_id", inst.Id,
		"subject", subject,
		"direction", cpReq.Direction,
		"guest_path", cpReq.GuestPath,
	)

	var cpErr error
	var bytesTransferred int64
	switch cpReq.Direction {
	case "to":
		bytesTransferred, cpErr = s.handleCopyTo(ctx, ws, inst, cpReq)
	case "from":
		bytesTransferred, cpErr = s.handleCopyFrom(ctx, ws, inst, cpReq)
	default:
		cpErr = fmt.Errorf("invalid direction: %s (must be 'to' or 'from')", cpReq.Direction)
	}

	duration := time.Since(startTime)
	success := cpErr == nil

	// Record metrics
	if guest.GuestMetrics != nil {
		guest.GuestMetrics.RecordCpSession(ctx, startTime, cpReq.Direction, success, bytesTransferred)
	}

	// Update span with result
	span.SetAttributes(
		attribute.Int64("bytes_transferred", bytesTransferred),
		attribute.Bool("success", success),
	)

	if cpErr != nil {
		span.RecordError(cpErr)
		span.SetStatus(codes.Error, cpErr.Error())
		log.ErrorContext(ctx, "cp failed",
			"error", cpErr,
			"instance_id", inst.Id,
			"subject", subject,
			"duration_ms", duration.Milliseconds(),
		)
		// Only send error message if it hasn't already been sent to the client
		var sentErr *cpErrorSent
		if !errors.As(cpErr, &sentErr) {
			errMsg, _ := json.Marshal(CpError{Type: "error", Message: cpErr.Error()})
			ws.WriteMessage(websocket.TextMessage, errMsg)
		}
		return
	}

	span.SetStatus(codes.Ok, "")
	log.InfoContext(ctx, "cp session ended",
		"instance_id", inst.Id,
		"subject", subject,
		"direction", cpReq.Direction,
		"duration_ms", duration.Milliseconds(),
		"bytes_transferred", bytesTransferred,
	)
}

// handleCopyTo handles copying files from client to guest
// Returns the number of bytes transferred and any error.
func (s *ApiService) handleCopyTo(ctx context.Context, ws *websocket.Conn, inst *instances.Instance, req CpRequest) (int64, error) {
	// Create vsock dialer for this hypervisor type
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return 0, fmt.Errorf("create vsock dialer: %w", err)
	}

	grpcConn, err := guest.GetOrCreateConn(ctx, dialer)
	if err != nil {
		return 0, fmt.Errorf("get grpc connection: %w", err)
	}

	client := guest.NewDRPCGuestServiceClient(grpcConn)
	stream, err := client.CopyToGuest(ctx)
	if err != nil {
		return 0, fmt.Errorf("start copy stream: %w", err)
	}

	// Send start message
	mode := req.Mode
	if mode == 0 {
		mode = 0644
		if req.IsDir {
			mode = 0755
		}
	}

	if err := stream.Send(&guest.CopyToGuestRequest{
		Request: &guest.CopyToGuestRequest_Start{
			Start: &guest.CopyToGuestStart{
				Path:  req.GuestPath,
				Mode:  mode,
				IsDir: req.IsDir,
				Uid:   req.Uid,
				Gid:   req.Gid,
			},
		},
	}); err != nil {
		return 0, fmt.Errorf("send start: %w", err)
	}

	// Read data chunks from WebSocket and forward to guest
	var receivedEndMessage bool
	var bytesSent int64
	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				break
			}
			return bytesSent, fmt.Errorf("read websocket: %w", err)
		}

		if msgType == websocket.TextMessage {
			// Check for end message
			var msg map[string]interface{}
			if json.Unmarshal(data, &msg) == nil {
				if msg["type"] == "end" {
					receivedEndMessage = true
					break
				}
			}
		} else if msgType == websocket.BinaryMessage {
			// Forward data chunk to guest
			if err := stream.Send(&guest.CopyToGuestRequest{
				Request: &guest.CopyToGuestRequest_Data{Data: data},
			}); err != nil {
				return bytesSent, fmt.Errorf("send data: %w", err)
			}
			bytesSent += int64(len(data))
		}
	}

	// If the WebSocket closed without receiving an end message, the transfer is incomplete
	if !receivedEndMessage {
		return bytesSent, fmt.Errorf("client disconnected before completing transfer")
	}

	// Send end message to guest
	if err := stream.Send(&guest.CopyToGuestRequest{
		Request: &guest.CopyToGuestRequest_End{End: &guest.CopyToGuestEnd{}},
	}); err != nil {
		return bytesSent, fmt.Errorf("send end: %w", err)
	}

	// Get response
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return bytesSent, fmt.Errorf("close stream: %w", err)
	}

	// Send result to client
	result := CpResult{
		Type:         "result",
		Success:      resp.Success,
		Error:        resp.Error,
		BytesWritten: resp.BytesWritten,
	}
	resultJSON, _ := json.Marshal(result)
	ws.WriteMessage(websocket.TextMessage, resultJSON)

	if !resp.Success {
		// Return a wrapped error so the caller logs it correctly but doesn't send a duplicate
		return resp.BytesWritten, &cpErrorSent{err: fmt.Errorf("copy to guest failed: %s", resp.Error)}
	}
	return resp.BytesWritten, nil
}

// handleCopyFrom handles copying files from guest to client
// Returns the number of bytes transferred and any error.
func (s *ApiService) handleCopyFrom(ctx context.Context, ws *websocket.Conn, inst *instances.Instance, req CpRequest) (int64, error) {
	// Create vsock dialer for this hypervisor type
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return 0, fmt.Errorf("create vsock dialer: %w", err)
	}

	grpcConn, err := guest.GetOrCreateConn(ctx, dialer)
	if err != nil {
		return 0, fmt.Errorf("get grpc connection: %w", err)
	}

	client := guest.NewDRPCGuestServiceClient(grpcConn)
	stream, err := client.CopyFromGuest(ctx, &guest.CopyFromGuestRequest{
		Path:        req.GuestPath,
		FollowLinks: req.FollowLinks,
	})
	if err != nil {
		return 0, fmt.Errorf("start copy stream: %w", err)
	}

	var receivedFinal bool
	var bytesReceived int64

	// Stream responses to WebSocket client
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return bytesReceived, fmt.Errorf("receive: %w", err)
		}

		switch r := resp.Response.(type) {
		case *guest.CopyFromGuestResponse_Header:
			header := CpFileHeader{
				Type:       "header",
				Path:       r.Header.Path,
				Mode:       r.Header.Mode,
				IsDir:      r.Header.IsDir,
				IsSymlink:  r.Header.IsSymlink,
				LinkTarget: r.Header.LinkTarget,
				Size:       r.Header.Size,
				Mtime:      r.Header.Mtime,
				Uid:        r.Header.Uid,
				Gid:        r.Header.Gid,
			}
			headerJSON, _ := json.Marshal(header)
			if err := ws.WriteMessage(websocket.TextMessage, headerJSON); err != nil {
				return bytesReceived, fmt.Errorf("write header: %w", err)
			}

		case *guest.CopyFromGuestResponse_Data:
			if err := ws.WriteMessage(websocket.BinaryMessage, r.Data); err != nil {
				return bytesReceived, fmt.Errorf("write data: %w", err)
			}
			bytesReceived += int64(len(r.Data))

		case *guest.CopyFromGuestResponse_End:
			endMarker := CpEndMarker{
				Type:  "end",
				Final: r.End.Final,
			}
			endJSON, _ := json.Marshal(endMarker)
			if err := ws.WriteMessage(websocket.TextMessage, endJSON); err != nil {
				return bytesReceived, fmt.Errorf("write end: %w", err)
			}
			if r.End.Final {
				receivedFinal = true
				return bytesReceived, nil
			}

		case *guest.CopyFromGuestResponse_Error:
			cpErr := CpError{
				Type:    "error",
				Message: r.Error.Message,
				Path:    r.Error.Path,
			}
			errJSON, _ := json.Marshal(cpErr)
			ws.WriteMessage(websocket.TextMessage, errJSON)
			// Return a wrapped error so the caller logs it correctly but doesn't send a duplicate
			return bytesReceived, &cpErrorSent{err: fmt.Errorf("copy from guest failed: %s", r.Error.Message)}
		}
	}

	if !receivedFinal {
		return bytesReceived, fmt.Errorf("copy stream ended without completion marker")
	}
	return bytesReceived, nil
}
