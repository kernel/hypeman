package builds

import (
	"context"
)

const (
	// BuildAgentVsockPort is the port the builder agent listens on inside the guest
	BuildAgentVsockPort = 5001

	// SecretsVsockPort is the port the host listens on for secret requests from builder agents
	SecretsVsockPort = 5002
)

// VsockMessage is the envelope for vsock communication with builder agents
type VsockMessage struct {
	Type      string            `json:"type"`
	Result    *BuildResult      `json:"result,omitempty"`
	Log       string            `json:"log,omitempty"`
	SecretIDs []string          `json:"secret_ids,omitempty"` // For secrets request
	Secrets   map[string]string `json:"secrets,omitempty"`    // For secrets response
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
