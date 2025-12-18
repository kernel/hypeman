package builds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/mdlayher/vsock"
)

const (
	// BuildAgentVsockPort is the port the builder agent listens on
	BuildAgentVsockPort = 5001
)

// VsockMessage is the envelope for vsock communication with builder agents
type VsockMessage struct {
	Type   string       `json:"type"`
	Result *BuildResult `json:"result,omitempty"`
	Log    string       `json:"log,omitempty"`
}

// SecretsRequest is sent by the builder agent to fetch secrets
type SecretsRequest struct {
	SecretIDs []string `json:"secret_ids"`
}

// SecretsResponse contains the requested secrets
type SecretsResponse struct {
	Secrets map[string]string `json:"secrets"`
}

// SecretProvider provides secrets for builds
type SecretProvider interface {
	// GetSecrets returns the values for the given secret IDs
	GetSecrets(ctx context.Context, secretIDs []string) (map[string]string, error)
}

// NoOpSecretProvider returns empty secrets (for builds without secrets)
type NoOpSecretProvider struct{}

func (p *NoOpSecretProvider) GetSecrets(ctx context.Context, secretIDs []string) (map[string]string, error) {
	return make(map[string]string), nil
}

// BuildResultHandler is called when a build completes
type BuildResultHandler func(result *BuildResult)

// BuildLogHandler is called for each log line from the builder
type BuildLogHandler func(line string)

// VsockHandler handles vsock communication with builder agents
type VsockHandler struct {
	secretProvider SecretProvider
	resultHandlers map[string]BuildResultHandler
	logHandlers    map[string]BuildLogHandler
	mu             sync.RWMutex
	logger         *slog.Logger
}

// NewVsockHandler creates a new vsock handler
func NewVsockHandler(secretProvider SecretProvider, logger *slog.Logger) *VsockHandler {
	if secretProvider == nil {
		secretProvider = &NoOpSecretProvider{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &VsockHandler{
		secretProvider: secretProvider,
		resultHandlers: make(map[string]BuildResultHandler),
		logHandlers:    make(map[string]BuildLogHandler),
		logger:         logger,
	}
}

// RegisterHandlers registers handlers for a specific build
func (h *VsockHandler) RegisterHandlers(buildID string, resultHandler BuildResultHandler, logHandler BuildLogHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if resultHandler != nil {
		h.resultHandlers[buildID] = resultHandler
	}
	if logHandler != nil {
		h.logHandlers[buildID] = logHandler
	}
}

// UnregisterHandlers removes handlers for a build
func (h *VsockHandler) UnregisterHandlers(buildID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.resultHandlers, buildID)
	delete(h.logHandlers, buildID)
}

// ListenAndServe starts listening for vsock connections
// This should be called once and runs until the context is cancelled
func (h *VsockHandler) ListenAndServe(ctx context.Context) error {
	l, err := vsock.Listen(BuildAgentVsockPort, nil)
	if err != nil {
		return fmt.Errorf("listen vsock: %w", err)
	}
	defer l.Close()

	h.logger.Info("vsock handler listening", "port", BuildAgentVsockPort)

	// Handle context cancellation
	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h.logger.Error("accept vsock connection", "error", err)
			continue
		}
		go h.handleConnection(ctx, conn)
	}
}

// handleConnection handles a single vsock connection
func (h *VsockHandler) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var msg VsockMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				return
			}
			h.logger.Error("decode vsock message", "error", err)
			return
		}

		switch msg.Type {
		case "get_secrets":
			// Decode the actual request
			var req SecretsRequest
			// Re-read to get the full message - for simplicity we expect
			// the secrets list in a separate field or we can use the same connection
			secrets, err := h.secretProvider.GetSecrets(ctx, req.SecretIDs)
			if err != nil {
				h.logger.Error("get secrets", "error", err)
				encoder.Encode(SecretsResponse{Secrets: make(map[string]string)})
				continue
			}
			encoder.Encode(SecretsResponse{Secrets: secrets})

		case "build_result":
			if msg.Result != nil {
				h.handleBuildResult(msg.Result)
			}

		case "log":
			if msg.Log != "" {
				h.handleLog(msg.Log)
			}

		default:
			h.logger.Warn("unknown vsock message type", "type", msg.Type)
		}
	}
}

// handleBuildResult dispatches a build result to the registered handler
func (h *VsockHandler) handleBuildResult(result *BuildResult) {
	// For now, we broadcast to all handlers since we don't have build ID in the message
	// In a production system, you'd include the build ID in the result
	h.mu.RLock()
	handlers := make([]BuildResultHandler, 0, len(h.resultHandlers))
	for _, handler := range h.resultHandlers {
		handlers = append(handlers, handler)
	}
	h.mu.RUnlock()

	for _, handler := range handlers {
		handler(result)
	}
}

// handleLog dispatches a log line to the registered handler
func (h *VsockHandler) handleLog(line string) {
	h.mu.RLock()
	handlers := make([]BuildLogHandler, 0, len(h.logHandlers))
	for _, handler := range h.logHandlers {
		handlers = append(handlers, handler)
	}
	h.mu.RUnlock()

	for _, handler := range handlers {
		handler(line)
	}
}

// ConnectToBuilder connects to a builder agent via vsock
// This is used to communicate with a specific builder VM
func ConnectToBuilder(cid uint32) (net.Conn, error) {
	return vsock.Dial(cid, BuildAgentVsockPort, nil)
}

// WaitForBuildResult waits for a build result from a specific builder
// It connects to the builder's vsock and reads the result
func WaitForBuildResult(ctx context.Context, cid uint32) (*BuildResult, error) {
	conn, err := vsock.Dial(cid, BuildAgentVsockPort, nil)
	if err != nil {
		return nil, fmt.Errorf("dial builder: %w", err)
	}
	defer conn.Close()

	// Set read deadline based on context
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetReadDeadline(deadline)
	}

	decoder := json.NewDecoder(conn)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var msg VsockMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				continue
			}
			return nil, fmt.Errorf("decode message: %w", err)
		}

		if msg.Type == "build_result" && msg.Result != nil {
			return msg.Result, nil
		}
	}
}

