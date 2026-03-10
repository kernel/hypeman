//go:build linux

package ingress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

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

	lockFile, err := os.OpenFile(extractPath+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return "", fmt.Errorf("open extraction lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("lock extraction: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// Another process may have extracted it while we waited for the lock.
	if _, err := os.Stat(extractPath); err == nil {
		if storedHash, err := os.ReadFile(hashPath); err == nil && string(storedHash) == embeddedHash {
			return extractPath, nil
		}
	}

	if err := atomicWriteExecutable(extractPath, data); err != nil {
		return "", fmt.Errorf("write caddy binary: %w", err)
	}

	// Write hash file for future comparisons
	if err := atomicWriteFile(hashPath, []byte(embeddedHash), 0644); err != nil {
		// Non-fatal - binary is extracted, just won't have hash for next time
		// This could cause unnecessary re-extractions but won't break functionality
		slog.Warn("failed to write caddy binary hash file", "path", hashPath, "error", err)
	}

	return extractPath, nil
}

// GetCaddyBinaryPath returns path to extracted binary, extracting if needed.
func GetCaddyBinaryPath(p *paths.Paths) (string, error) {
	return ExtractCaddyBinary(p)
}

func atomicWriteExecutable(path string, data []byte) error {
	return atomicWriteFile(path, data, 0755)
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, "caddy-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Chmod(mode); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install file: %w", err)
	}
	cleanupTmp = false
	return nil
}
