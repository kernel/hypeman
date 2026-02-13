package instances

import (
	"bufio"
	"context"
	"fmt"
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

		// VM is stopped -- lazily parse exit info from serial console log
		// if not already populated. This runs once when the state is first
		// queried after the VM dies.
		if stored.ExitCode == nil {
			m.parseExitSentinel(ctx, stored)
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
	return inst
}

// parseExitSentinel reads the last lines of the serial console log to find the
// HYPEMAN-EXIT sentinel written by init before shutdown. If found, it persists
// the exit code and message to metadata so subsequent queries don't re-parse.
func (m *manager) parseExitSentinel(ctx context.Context, stored *StoredMetadata) {
	log := logger.FromContext(ctx)

	logPath := m.paths.InstanceAppLog(stored.Id)
	f, err := os.Open(logPath)
	if err != nil {
		return // Log file doesn't exist, nothing to parse
	}
	defer f.Close()

	// Scan the file looking for the sentinel line.
	// The sentinel is near the end of the file (written just before reboot),
	// but we scan from the beginning since log files are typically small
	// (serial console output, not application logs).
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		code, msg, ok := parseExitSentinelLine(line)
		if ok {
			stored.ExitCode = &code
			stored.ExitMessage = msg

			// Persist to metadata so we don't re-parse next time
			meta := &metadata{StoredMetadata: *stored}
			if err := m.saveMetadata(meta); err != nil {
				log.WarnContext(ctx, "failed to persist exit info", "instance_id", stored.Id, "error", err)
			} else {
				log.DebugContext(ctx, "parsed exit info from serial log", "instance_id", stored.Id, "exit_code", code, "exit_message", msg)
			}
			return
		}
	}
}

// parseExitSentinelLine parses a single log line looking for the HYPEMAN-EXIT sentinel.
// The sentinel format is embedded in a log line like:
// 2026-02-13T15:26:27Z [INFO] [hypeman-init:entrypoint] HYPEMAN-EXIT code=127 message="command not found"
// Returns the exit code, message, and whether parsing was successful.
func parseExitSentinelLine(line string) (int, string, bool) {
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
