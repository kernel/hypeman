package ingress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman/lib/paths"
)

// CaddyVersion is the version of Caddy embedded in this build.
const CaddyVersion = "v2.10.2"

// caddyBinaryFS and caddyArch are defined in architecture-specific files:
// - binaries_amd64.go (for x86_64)
// - binaries_arm64.go (for aarch64)

// ExtractCaddyBinary extracts the embedded Caddy binary to the data directory.
// Returns the path to the extracted binary.
// If the binary already exists but doesn't match the embedded version (e.g., after
// rebuilding with different modules), it will be re-extracted.
func ExtractCaddyBinary(p *paths.Paths) (string, error) {
	embeddedPath := fmt.Sprintf("binaries/caddy/%s/%s/caddy", CaddyVersion, caddyArch)
	extractPath := p.CaddyBinary(CaddyVersion, caddyArch)
	hashPath := extractPath + ".sha256"

	// Read embedded binary
	data, err := caddyBinaryFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("read embedded caddy binary: %w", err)
	}

	// Compute hash of embedded binary
	hash := sha256.Sum256(data)
	embeddedHash := hex.EncodeToString(hash[:])

	// Check if already extracted with matching hash
	if _, err := os.Stat(extractPath); err == nil {
		// Binary exists, check if hash matches
		if storedHash, err := os.ReadFile(hashPath); err == nil {
			if string(storedHash) == embeddedHash {
				// Hash matches, use existing binary
				return extractPath, nil
			}
			// Hash mismatch - need to re-extract (binary was rebuilt with different modules)
		}
		// No hash file or mismatch - re-extract
	}

	// Create directory
	if err := os.MkdirAll(filepath.Dir(extractPath), 0755); err != nil {
		return "", fmt.Errorf("create caddy binary dir: %w", err)
	}

	// Write binary to filesystem
	if err := os.WriteFile(extractPath, data, 0755); err != nil {
		return "", fmt.Errorf("write caddy binary: %w", err)
	}

	// Write hash file for future comparisons
	if err := os.WriteFile(hashPath, []byte(embeddedHash), 0644); err != nil {
		// Non-fatal - binary is extracted, just won't have hash for next time
		// This could cause unnecessary re-extractions but won't break functionality
		slog.Info("failed to write caddy binary hash file", "path", hashPath, "error", err)
	}

	return extractPath, nil
}

// GetCaddyBinaryPath returns path to extracted binary, extracting if needed.
func GetCaddyBinaryPath(p *paths.Paths) (string, error) {
	return ExtractCaddyBinary(p)
}
