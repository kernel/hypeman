// Package forkidentity produces and persists per-fork identity records
// for firecracker (and other) snapshot fan-out forks. When N forks share
// a single restored memory image, every fork comes up with identical
// entropy state, machine-id, hostname, and clock — which is unsafe for
// crypto and confusing for users. This package generates a small JSON
// record (random seed bytes, clock offset, fork id) and writes it into
// the fork's data directory at a known path. A guest agent reads the
// file at boot and applies it: reseeds /dev/urandom, sets a fresh
// machine-id, and steps the clock forward. The hypervisor side never
// touches guest state directly.
//
// This package is intentionally self-contained and side-effect-free
// outside of the explicit Write call so it can be composed by every
// hypervisor backend that supports snapshot forks.
package forkidentity

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileName is the canonical filename written into a fork's data dir.
// Guest agents that consume identity records read from this name.
const FileName = "fork-identity.json"

// EntropySeedBytes is the size of the per-fork random seed in bytes.
// 256 bytes (2048 bits) is well past what any reasonable kernel RNG
// reseed would consume; keeping it large means we can split the buffer
// into several pools (urandom, machine-id, hostname salt) without
// running out.
const EntropySeedBytes = 256

// Identity is the on-disk record. Field tags are stable; bumping
// Version invalidates older records.
type Identity struct {
	Version       int       `json:"version"`
	ForkID        string    `json:"fork_id"`
	EntropySeed   []byte    `json:"entropy_seed"`
	ClockOffsetNs int64     `json:"clock_offset_ns"`
	CreatedAt     time.Time `json:"created_at"`
}

// CurrentVersion is bumped whenever the on-disk format changes in a
// guest-agent-incompatible way.
const CurrentVersion = 1

// ErrEmpty is returned by Read when the file is missing.
var ErrEmpty = errors.New("forkidentity: identity file not present")

// Build generates a fresh identity for forkID. It pulls EntropySeedBytes
// of cryptographic randomness and derives a small clock offset from the
// first 8 bytes so that the guest can step its clock forward without an
// extra syscall.
//
// Build never errors except on rand.Reader exhaustion, which on Linux
// means the kernel CSPRNG is broken — propagate it.
func Build(forkID string) (Identity, error) {
	if forkID == "" {
		return Identity{}, errors.New("forkidentity: fork id is required")
	}
	seed := make([]byte, EntropySeedBytes)
	if _, err := rand.Read(seed); err != nil {
		return Identity{}, fmt.Errorf("forkidentity: read random seed: %w", err)
	}
	// 0..~16 ms of forward jitter. Enough to break clock-based
	// correlation across forks without measurably affecting wall time.
	jitter := int64(binary.LittleEndian.Uint64(seed[:8]) % uint64(16*time.Millisecond))
	return Identity{
		Version:       CurrentVersion,
		ForkID:        forkID,
		EntropySeed:   seed,
		ClockOffsetNs: jitter,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

// Write atomically persists id under dir/<FileName>. The dir must
// already exist; callers typically pass the fork's data directory.
func Write(dir string, id Identity) error {
	if id.Version == 0 {
		return errors.New("forkidentity: refusing to write zero-versioned identity")
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("forkidentity: marshal: %w", err)
	}
	full := filepath.Join(dir, FileName)
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("forkidentity: write tmp: %w", err)
	}
	if err := os.Rename(tmp, full); err != nil {
		return fmt.Errorf("forkidentity: rename: %w", err)
	}
	return nil
}

// Read loads the identity record from dir/<FileName>. ErrEmpty when the
// file does not exist.
func Read(dir string) (Identity, error) {
	full := filepath.Join(dir, FileName)
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Identity{}, ErrEmpty
		}
		return Identity{}, fmt.Errorf("forkidentity: read: %w", err)
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil {
		return Identity{}, fmt.Errorf("forkidentity: unmarshal: %w", err)
	}
	return id, nil
}
