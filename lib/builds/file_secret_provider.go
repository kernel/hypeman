package builds

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileSecretProvider reads secrets from files in a directory.
// Each secret is stored as a file named by its ID, with the secret value as the file content.
// Example: /etc/hypeman/secrets/npm_token contains the npm token value.
type FileSecretProvider struct {
	secretsDir string
}

// NewFileSecretProvider creates a new file-based secret provider.
// secretsDir is the directory containing secret files (e.g., /etc/hypeman/secrets/).
func NewFileSecretProvider(secretsDir string) *FileSecretProvider {
	return &FileSecretProvider{
		secretsDir: secretsDir,
	}
}

// GetSecrets returns the values for the given secret IDs by reading files from the secrets directory.
// Missing secrets are silently skipped (not an error).
// Returns an error only if a secret file exists but cannot be read.
func (p *FileSecretProvider) GetSecrets(ctx context.Context, secretIDs []string) (map[string]string, error) {
	result := make(map[string]string)

	for _, id := range secretIDs {
		// Validate secret ID to prevent path traversal
		if strings.Contains(id, "/") || strings.Contains(id, "\\") || id == ".." || id == "." {
			continue // Skip invalid IDs
		}

		path := filepath.Join(p.secretsDir, id)

		// Check context before each file read
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// Secret doesn't exist - skip it (not an error)
				continue
			}
			return nil, fmt.Errorf("read secret %s: %w", id, err)
		}

		// Trim whitespace (especially trailing newlines)
		result[id] = strings.TrimSpace(string(data))
	}

	return result, nil
}

// Ensure FileSecretProvider implements SecretProvider
var _ SecretProvider = (*FileSecretProvider)(nil)

