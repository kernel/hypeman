package instances

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

// exitSentinelPrefix is the machine-parseable prefix written by init to serial console.
const exitSentinelPrefix = "HYPEMAN-EXIT "

// stateResult holds the result of state derivation
type stateResult struct {
	State State
	Error *string // Non-nil if state couldn't be determined
}

// deriveState determines instance state by checking socket and querying the hypervisor.
// Returns StateUnknown with an error message if the socket exists but hypervisor is unreachable.
func (m *manager) deriveState(ctx context.Context, stored *StoredMetadata) stateResult {
	log := logger.FromContext(ctx)

	// 1. Check if socket exists
	if _, err := os.Stat(stored.SocketPath); err != nil {
		// No socket - check for snapshot to distinguish Stopped vs Standby
		if m.hasSnapshot(stored.DataDir) {
			return stateResult{State: StateStandby}
		}
		return stateResult{State: StateStopped}
	}

	// 2. Socket exists - query hypervisor for actual state
	hv, err := m.getHypervisor(stored.SocketPath, stored.HypervisorType)
	if err != nil {
		// Failed to create client - this is unexpected if socket exists
		errMsg := fmt.Sprintf("failed to create hypervisor client: %v", err)
		log.WarnContext(ctx, "failed to determine instance state",
			"instance_id", stored.Id,
			"socket", stored.SocketPath,
			"error", err,
		)
		return stateResult{State: StateUnknown, Error: &errMsg}
	}

	info, err := hv.GetVMInfo(ctx)
	if err != nil {
		// Socket exists but hypervisor is unreachable - this is unexpected
		errMsg := fmt.Sprintf("failed to query hypervisor: %v", err)
		log.WarnContext(ctx, "failed to query hypervisor state",
			"instance_id", stored.Id,
			"socket", stored.SocketPath,
			"error", err,
		)
		return stateResult{State: StateUnknown, Error: &errMsg}
	}

	// 3. Map hypervisor state to our state
	switch info.State {
	case hypervisor.StateCreated:
		return stateResult{State: StateCreated}
	case hypervisor.StateRunning:
		return stateResult{State: StateRunning}
	case hypervisor.StatePaused:
		return stateResult{State: StatePaused}
	case hypervisor.StateShutdown:
		return stateResult{State: StateShutdown}
	default:
		// Unknown state - log and return Unknown
		errMsg := fmt.Sprintf("unexpected hypervisor state: %s", info.State)
		log.WarnContext(ctx, "hypervisor returned unexpected state",
			"instance_id", stored.Id,
			"hypervisor_state", info.State,
		)
		return stateResult{State: StateUnknown, Error: &errMsg}
	}
}

// hasSnapshot checks if a snapshot exists for an instance
func (m *manager) hasSnapshot(dataDir string) bool {
	snapshotDir := filepath.Join(dataDir, "snapshots", "snapshot-latest")
	info, err := os.Stat(snapshotDir)
	if err != nil {
		return false
	}
	// Check directory exists and is not empty
	if !info.IsDir() {
		return false
	}
	// Read directory to check for any snapshot files
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// toInstance converts stored metadata to Instance with derived fields
func (m *manager) toInstance(ctx context.Context, meta *metadata) Instance {
	result := m.deriveState(ctx, &meta.StoredMetadata)
	inst := Instance{
		StoredMetadata: meta.StoredMetadata,
		State:          result.State,
		StateError:     result.Error,
		HasSnapshot:    m.hasSnapshot(meta.StoredMetadata.DataDir),
	}

	// If VM is stopped and exit info isn't persisted yet, populate in-memory
	// from the serial console log. This is read-only -- no metadata writes.
	// Persistence happens under lock in stopInstance or persistExitInfo.
	if inst.State == StateStopped && inst.ExitCode == nil {
		if code, msg, ok := m.parseExitSentinel(inst.Id); ok {
			inst.ExitCode = &code
			inst.ExitMessage = msg
		}
	}

	return inst
}

// parseExitSentinel reads the last lines of the serial console log to find the
// HYPEMAN-EXIT sentinel written by init before shutdown.
// Returns the exit code, message, and whether a sentinel was found.
// This is a pure reader with no side effects.
func (m *manager) parseExitSentinel(id string) (int, string, bool) {
	logPath := m.paths.InstanceAppLog(id)

	// Read the tail of the log file. The sentinel is written near the end
	// (just before reboot), so we only need the last few KB even if the
	// serial console log is large from a chatty app.
	const tailSize = 8192
	data, err := readTail(logPath, tailSize)
	if err != nil {
		return 0, "", false
	}

	// Scan lines from the tail looking for the sentinel
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		code, msg, ok := parseExitSentinelLine(line)
		if ok {
			return code, msg, true
		}
	}
	return 0, "", false
}

// persistExitInfo parses exit info from the serial console and persists it to
// metadata. Must be called under the instance lock.
func (m *manager) persistExitInfo(ctx context.Context, id string) {
	log := logger.FromContext(ctx)

	meta, err := m.loadMetadata(id)
	if err != nil {
		return
	}

	// Already persisted
	if meta.ExitCode != nil {
		return
	}

	code, msg, ok := m.parseExitSentinel(id)
	if !ok {
		return
	}

	meta.ExitCode = &code
	meta.ExitMessage = msg
	if err := m.saveMetadata(meta); err != nil {
		log.WarnContext(ctx, "failed to persist exit info", "instance_id", id, "error", err)
	} else {
		log.DebugContext(ctx, "parsed exit info from serial log", "instance_id", id, "exit_code", code, "exit_message", msg)
	}
}

// readTail reads the last n bytes of a file. If the file is smaller than n,
// the entire file is returned.
func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	offset := info.Size() - n
	if offset < 0 {
		offset = 0
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}

	return io.ReadAll(f)
}

// parseExitSentinelLine parses a single log line looking for the HYPEMAN-EXIT sentinel.
// The sentinel format is embedded in a log line like:
// 2026-02-13T15:26:27Z [INFO] [hypeman-init:entrypoint] HYPEMAN-EXIT code=127 message="command not found"
// Returns the exit code, message, and whether parsing was successful.
func parseExitSentinelLine(line string) (int, string, bool) {
	// Strip whitespace -- serial console (TTY) adds \r to line endings
	line = strings.TrimSpace(line)

	idx := strings.Index(line, exitSentinelPrefix)
	if idx < 0 {
		return 0, "", false
	}

	// Extract the part after "HYPEMAN-EXIT "
	sentinel := line[idx+len(exitSentinelPrefix):]

	// Parse code=N
	if !strings.HasPrefix(sentinel, "code=") {
		return 0, "", false
	}
	sentinel = sentinel[5:] // skip "code="

	// Find the end of the code number
	spaceIdx := strings.Index(sentinel, " ")
	if spaceIdx < 0 {
		// Just a code, no message
		code, err := strconv.Atoi(sentinel)
		if err != nil {
			return 0, "", false
		}
		return code, "", true
	}

	code, err := strconv.Atoi(sentinel[:spaceIdx])
	if err != nil {
		return 0, "", false
	}

	// Parse message="..."
	rest := sentinel[spaceIdx+1:]
	if strings.HasPrefix(rest, "message=") {
		msgStr := rest[8:] // skip "message="
		// Unquote the message (it's Go-quoted via %q)
		if unquoted, err := strconv.Unquote(msgStr); err == nil {
			return code, unquoted, true
		}
		// If unquoting fails, use raw value (strip quotes if present)
		return code, strings.Trim(msgStr, "\""), true
	}

	return code, "", true
}

// listInstances returns all instances
func (m *manager) listInstances(ctx context.Context) ([]Instance, error) {
	log := logger.FromContext(ctx)
	log.DebugContext(ctx, "listing all instances")

	files, err := m.listMetadataFiles()
	if err != nil {
		log.ErrorContext(ctx, "failed to list metadata files", "error", err)
		return nil, err
	}

	result := make([]Instance, 0, len(files))
	for _, file := range files {
		// Extract instance ID from path
		// Path format: {dataDir}/guests/{id}/metadata.json
		id := filepath.Base(filepath.Dir(file))

		meta, err := m.loadMetadata(id)
		if err != nil {
			// Skip instances with invalid metadata
			log.WarnContext(ctx, "skipping instance with invalid metadata", "instance_id", id, "error", err)
			continue
		}

		inst := m.toInstance(ctx, meta)
		result = append(result, inst)
	}

	log.DebugContext(ctx, "listed instances", "count", len(result))
	return result, nil
}

// getInstance returns a single instance by ID
func (m *manager) getInstance(ctx context.Context, id string) (*Instance, error) {
	log := logger.FromContext(ctx)
	log.DebugContext(ctx, "getting instance", "lookup", id)

	meta, err := m.loadMetadata(id)
	if err != nil {
		log.DebugContext(ctx, "failed to load instance metadata", "lookup", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	log.DebugContext(ctx, "retrieved instance", "instance_id", inst.Id, "state", inst.State)
	return &inst, nil
}
