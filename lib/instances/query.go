package instances

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

// exitSentinelPrefix is the machine-parseable prefix written by init to serial console.
const (
	exitSentinelPrefix         = "HYPEMAN-EXIT "
	programStartSentinelPrefix = "HYPEMAN-PROGRAM-START "
	agentReadySentinelPrefix   = "HYPEMAN-AGENT-READY "
	bootMarkerRescanInterval   = 1 * time.Second
)

// stateResult holds the result of state derivation
type stateResult struct {
	State               State
	Error               *string // Non-nil if state couldn't be determined
	BootMarkersHydrated bool
}

// deriveState determines instance state by checking socket and querying the hypervisor.
// Returns StateUnknown with an error message if the socket exists but hypervisor is unreachable.
func (m *manager) deriveState(ctx context.Context, stored *StoredMetadata) stateResult {
	return m.deriveStateWithOptions(ctx, stored, true)
}

// deriveStateWithoutHydration determines instance state without scanning serial logs
// to hydrate missing boot markers.
func (m *manager) deriveStateWithoutHydration(ctx context.Context, stored *StoredMetadata) stateResult {
	return m.deriveStateWithOptions(ctx, stored, false)
}

func (m *manager) deriveStateWithOptions(ctx context.Context, stored *StoredMetadata, hydrateBootMarkers bool) stateResult {
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
		hydrated := false
		if hydrateBootMarkers {
			hydrated = m.hydrateBootMarkersFromLogs(stored)
		}
		return stateResult{
			State:               deriveRunningState(stored),
			BootMarkersHydrated: hydrated,
		}
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

func deriveRunningState(stored *StoredMetadata) State {
	if stored.ProgramStartedAt == nil {
		return StateInitializing
	}
	if stored.SkipGuestAgent {
		return StateRunning
	}
	if stored.GuestAgentReadyAt == nil {
		return StateInitializing
	}
	return StateRunning
}

// hydrateBootMarkersFromLogs fills missing boot markers from serial logs.
// Returns true when at least one missing marker was found and populated.
func (m *manager) hydrateBootMarkersFromLogs(stored *StoredMetadata) bool {
	needProgram := stored.ProgramStartedAt == nil
	needAgent := !stored.SkipGuestAgent && stored.GuestAgentReadyAt == nil
	if !needProgram && !needAgent {
		m.clearBootMarkerRescan(stored.Id)
		return false
	}
	if !m.shouldScanBootMarkers(stored.Id) {
		return false
	}

	programStartedAt, guestAgentReadyAt := m.parseBootMarkers(stored.Id, needProgram, needAgent, stored.StartedAt)
	hydrated := false
	if needProgram && programStartedAt != nil {
		stored.ProgramStartedAt = programStartedAt
		hydrated = true
	}
	if needAgent && guestAgentReadyAt != nil {
		stored.GuestAgentReadyAt = guestAgentReadyAt
		hydrated = true
	}
	if hydrated {
		m.clearBootMarkerRescan(stored.Id)
	} else {
		m.deferBootMarkerRescan(stored.Id)
	}
	return hydrated
}

// parseBootMarkers scans app logs (including rotated files) and returns the
// newest observed program-start and guest-agent-ready marker timestamps.
// When startedAt is provided, files last modified before this boot start are ignored.
func (m *manager) parseBootMarkers(id string, needProgram bool, needAgent bool, startedAt *time.Time) (*time.Time, *time.Time) {
	logPaths := m.appLogPathsForMarkerScan(id)

	var programStartedAt *time.Time
	var guestAgentReadyAt *time.Time
	// Iterate newest-to-oldest so we can stop once all required markers are found.
	for i := len(logPaths) - 1; i >= 0; i-- {
		logPath := logPaths[i]
		if !fileMayContainCurrentBootMarkers(logPath, startedAt) {
			continue
		}

		f, err := os.Open(logPath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if ts, ok := parseProgramStartSentinelLine(line); ok {
				if programStartedAt == nil || ts.After(*programStartedAt) {
					t := ts
					programStartedAt = &t
				}
			}
			if ts, ok := parseAgentReadySentinelLine(line); ok {
				if guestAgentReadyAt == nil || ts.After(*guestAgentReadyAt) {
					t := ts
					guestAgentReadyAt = &t
				}
			}
		}
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			continue
		}
		if (!needProgram || programStartedAt != nil) && (!needAgent || guestAgentReadyAt != nil) {
			return programStartedAt, guestAgentReadyAt
		}
	}

	return programStartedAt, guestAgentReadyAt
}

func fileMayContainCurrentBootMarkers(path string, startedAt *time.Time) bool {
	if startedAt == nil {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.ModTime().UTC().Before(startedAt.UTC())
}

func (m *manager) shouldScanBootMarkers(id string) bool {
	if nextAny, ok := m.bootMarkerScans.Load(id); ok {
		if next, ok := nextAny.(time.Time); ok && m.nowUTC().Before(next) {
			return false
		}
	}
	return true
}

func (m *manager) deferBootMarkerRescan(id string) {
	m.bootMarkerScans.Store(id, m.nowUTC().Add(bootMarkerRescanInterval))
}

func (m *manager) clearBootMarkerRescan(id string) {
	m.bootMarkerScans.Delete(id)
}

func (m *manager) nowUTC() time.Time {
	if m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

// appLogPathsForMarkerScan returns app log paths in chronological order
// (oldest rotated file to newest active file).
func (m *manager) appLogPathsForMarkerScan(id string) []string {
	base := m.paths.InstanceAppLog(id)
	rotatedMatches, err := filepath.Glob(base + ".*")
	if err != nil {
		return []string{base}
	}
	matches := append([]string{base}, rotatedMatches...)

	type logPathWithRank struct {
		path string
		rank int // higher rank means older rotated log; 0 means active file
	}
	paths := make([]logPathWithRank, 0, len(matches))
	for _, path := range matches {
		if path == base {
			paths = append(paths, logPathWithRank{path: path, rank: 0})
			continue
		}

		suffix := strings.TrimPrefix(path, base)
		if !strings.HasPrefix(suffix, ".") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(suffix, "."))
		if err != nil || n <= 0 {
			continue
		}
		paths = append(paths, logPathWithRank{path: path, rank: n})
	}

	if len(paths) == 0 {
		return []string{base}
	}

	slices.SortFunc(paths, func(a, b logPathWithRank) int {
		// Rotated logs first (older-to-newer by descending suffix), then active file.
		switch {
		case a.rank == 0 && b.rank != 0:
			return 1
		case a.rank != 0 && b.rank == 0:
			return -1
		case a.rank != b.rank:
			// Larger suffix is older and should be read first.
			return b.rank - a.rank
		default:
			return strings.Compare(a.path, b.path)
		}
	})

	ordered := make([]string, 0, len(paths))
	for _, p := range paths {
		ordered = append(ordered, p.path)
	}
	return ordered
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
	return m.toInstanceWithStateDerivation(ctx, meta, true)
}

func (m *manager) toInstanceWithoutHydration(ctx context.Context, meta *metadata) Instance {
	return m.toInstanceWithStateDerivation(ctx, meta, false)
}

func (m *manager) toInstanceWithStateDerivation(ctx context.Context, meta *metadata, hydrateBootMarkers bool) Instance {
	var result stateResult
	if hydrateBootMarkers {
		result = m.deriveState(ctx, &meta.StoredMetadata)
	} else {
		result = m.deriveStateWithoutHydration(ctx, &meta.StoredMetadata)
	}

	inst := Instance{
		StoredMetadata:      meta.StoredMetadata,
		State:               result.State,
		StateError:          result.Error,
		HasSnapshot:         m.hasSnapshot(meta.StoredMetadata.DataDir),
		BootMarkersHydrated: result.BootMarkersHydrated,
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
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
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

// persistBootMarkers parses program-start and guest-agent-ready markers from
// serial logs and persists them to metadata. Must be called under instance lock.
func (m *manager) persistBootMarkers(ctx context.Context, id string) {
	log := logger.FromContext(ctx)

	meta, err := m.loadMetadata(id)
	if err != nil {
		return
	}

	needProgram := meta.ProgramStartedAt == nil
	needAgent := !meta.SkipGuestAgent && meta.GuestAgentReadyAt == nil
	if !needProgram && !needAgent {
		return
	}

	programStartedAt, guestAgentReadyAt := m.parseBootMarkers(id, needProgram, needAgent, meta.StartedAt)
	updated := false
	if needProgram && programStartedAt != nil {
		meta.ProgramStartedAt = programStartedAt
		updated = true
	}
	if needAgent && guestAgentReadyAt != nil {
		meta.GuestAgentReadyAt = guestAgentReadyAt
		updated = true
	}
	if !updated {
		return
	}

	if err := m.saveMetadata(meta); err != nil {
		log.WarnContext(ctx, "failed to persist boot markers", "instance_id", id, "error", err)
	} else {
		log.DebugContext(ctx, "persisted boot markers from serial log", "instance_id", id)
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

func parseProgramStartSentinelLine(line string) (time.Time, bool) {
	return parseSentinelTimestamp(line, programStartSentinelPrefix)
}

func parseAgentReadySentinelLine(line string) (time.Time, bool) {
	return parseSentinelTimestamp(line, agentReadySentinelPrefix)
}

func parseSentinelTimestamp(line, sentinelPrefix string) (time.Time, bool) {
	line = strings.TrimSpace(line)

	idx := strings.Index(line, sentinelPrefix)
	if idx < 0 {
		return time.Time{}, false
	}

	sentinel := line[idx+len(sentinelPrefix):]
	for _, field := range strings.Fields(sentinel) {
		if !strings.HasPrefix(field, "ts=") {
			continue
		}
		ts := strings.TrimPrefix(field, "ts=")
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	}

	return time.Time{}, false
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
