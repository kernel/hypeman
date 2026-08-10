package builds

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateSecretID rejects secret IDs that are empty or could escape the
// secrets directory (path traversal). It is enforced both at the API
// boundary and by the file-based secret provider.
func ValidateSecretID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: must not be empty", ErrInvalidSecretID)
	}
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || id == ".." || id == "." {
		return fmt.Errorf("%w: %q must not contain path separators", ErrInvalidSecretID, id)
	}
	return nil
}

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
// Requested secrets are required: an invalid ID or a missing secret file is an
// error, so a build never proceeds with silently empty secrets.
func (p *FileSecretProvider) GetSecrets(ctx context.Context, secretIDs []string) (map[string]string, error) {
	result := make(map[string]string)

	for _, id := range secretIDs {
		// Validate secret ID to prevent path traversal
		if err := ValidateSecretID(id); err != nil {
			return nil, err
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
				return nil, fmt.Errorf("%w: %s", ErrSecretNotFound, id)
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
