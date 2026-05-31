//go:build linux

package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

const restoreDeepTraceEnv = "HYPEMAN_RESTORE_DEEP_TRACE"
const restoreDeepTraceDirEnv = "HYPEMAN_RESTORE_DEEP_TRACE_DIR"
const restoreDeepTraceEventsEnv = "HYPEMAN_RESTORE_DEEP_TRACE_EVENTS"

type restoreDeepTraceContextKey struct{}

type restoreDeepTrace struct {
	instanceID        string
	pid               int
	dir               string
	traceRoot         string
	traceMarker       *os.File
	previousTracingOn string
	enabledEvents     []string
	missingEvents     []string
	marks             []restoreDeepTraceMark
	samples           []restoreDeepTraceSample
	log               *slog.Logger
}

type restoreDeepTraceMark struct {
	Stage  string    `json:"stage"`
	Time   time.Time `json:"time"`
	Detail string    `json:"detail,omitempty"`
}

type restoreDeepTraceSample struct {
	Stage     string                   `json:"stage"`
	Time      time.Time                `json:"time"`
	Process   restoreDeepTraceThread   `json:"process"`
	IO        map[string]int64         `json:"io,omitempty"`
	Threads   []restoreDeepTraceThread `json:"threads"`
	ReadError string                   `json:"read_error,omitempty"`
}

type restoreDeepTraceThread struct {
	TID        int    `json:"tid"`
	Comm       string `json:"comm,omitempty"`
	State      string `json:"state,omitempty"`
	MinFaults  int64  `json:"min_faults"`
	MajFaults  int64  `json:"maj_faults"`
	UTimeTicks int64  `json:"utime_ticks"`
	STimeTicks int64  `json:"stime_ticks"`
	Threads    int64  `json:"threads,omitempty"`
	Processor  int64  `json:"processor,omitempty"`
}

type restoreDeepTraceSummary struct {
	InstanceID         string                                 `json:"instance_id"`
	PID                int                                    `json:"pid"`
	TraceRoot          string                                 `json:"trace_root"`
	TracePath          string                                 `json:"trace_path"`
	EnabledEvents      []string                               `json:"enabled_events"`
	MissingEvents      []string                               `json:"missing_events,omitempty"`
	Marks              []restoreDeepTraceMark                 `json:"marks"`
	Samples            []restoreDeepTraceSample               `json:"samples"`
	ProcessDeltas      map[string]restoreDeepTraceThreadDelta `json:"process_deltas,omitempty"`
	IODeltas           map[string]map[string]int64            `json:"io_deltas,omitempty"`
	ThreadDeltaSummary map[string]restoreDeepTraceThreadDelta `json:"thread_delta_summary,omitempty"`
}

type restoreDeepTraceThreadDelta struct {
	MinFaults  int64 `json:"min_faults"`
	MajFaults  int64 `json:"maj_faults"`
	UTimeTicks int64 `json:"utime_ticks"`
	STimeTicks int64 `json:"stime_ticks"`
}

func newRestoreDeepTrace(ctx context.Context, stored *StoredMetadata, pid int, snapshotDir string) (*restoreDeepTrace, error) {
	if strings.TrimSpace(os.Getenv(restoreDeepTraceEnv)) != "1" {
		return nil, nil
	}
	if stored == nil || pid <= 0 {
		return nil, nil
	}

	traceRoot, err := findRestoreDeepTraceRoot()
	if err != nil {
		return nil, err
	}
	baseDir := strings.TrimSpace(os.Getenv(restoreDeepTraceDirEnv))
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "hypeman-restore-debug")
	}
	dir := filepath.Join(baseDir, fmt.Sprintf("%s-%d", stored.Id, time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create restore deep trace dir: %w", err)
	}

	t := &restoreDeepTrace{
		instanceID: stored.Id,
		pid:        pid,
		dir:        dir,
		traceRoot:  traceRoot,
		log:        logger.FromContext(ctx),
	}
	meta := map[string]any{
		"instance_id":  stored.Id,
		"pid":          pid,
		"snapshot_dir": snapshotDir,
		"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
	}
	t.writeJSON("metadata.json", meta)

	if err := t.configureTracing(); err != nil {
		t.Close("configure_error", err)
		return nil, err
	}
	t.Mark("trace_start", "")
	t.Sample("trace_start")
	t.log.InfoContext(ctx, "restore deep trace started", "instance_id", stored.Id, "pid", pid, "dir", dir, "events", t.enabledEvents, "missing_events", t.missingEvents)
	return t, nil
}

func withRestoreDeepTrace(ctx context.Context, t *restoreDeepTrace) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, restoreDeepTraceContextKey{}, t)
}

func restoreDeepTraceFromContext(ctx context.Context) *restoreDeepTrace {
	t, _ := ctx.Value(restoreDeepTraceContextKey{}).(*restoreDeepTrace)
	return t
}

func (t *restoreDeepTrace) Mark(stage, detail string) {
	if t == nil {
		return
	}
	mark := restoreDeepTraceMark{
		Stage:  stage,
		Time:   time.Now().UTC(),
		Detail: detail,
	}
	t.marks = append(t.marks, mark)
	if t.traceMarker != nil {
		_, _ = fmt.Fprintf(t.traceMarker, "hypeman_%s instance=%s pid=%d %s\n", stage, t.instanceID, t.pid, detail)
	}
}

func (t *restoreDeepTrace) Sample(stage string) {
	if t == nil || t.pid <= 0 {
		return
	}
	sample := readRestoreDeepTraceSample(stage, t.pid)
	t.samples = append(t.samples, sample)
	t.writeJSON(fmt.Sprintf("proc_%02d_%s.json", len(t.samples), sanitizeDeepTraceName(stage)), sample)
}

func (t *restoreDeepTrace) Close(stage string, err error) {
	if t == nil {
		return
	}
	detail := ""
	if err != nil {
		detail = "error=" + sanitizeTraceMarkerValue(err.Error())
	}
	t.Mark(stage, detail)
	t.Sample(stage)

	_ = writeTracingFile(t.traceRoot, "tracing_on", "0")
	tracePath := filepath.Join(t.dir, "trace.txt")
	if data, readErr := os.ReadFile(filepath.Join(t.traceRoot, "trace")); readErr == nil {
		_ = os.WriteFile(tracePath, data, 0644)
	}

	if t.traceMarker != nil {
		_ = t.traceMarker.Close()
		t.traceMarker = nil
	}
	for _, event := range t.enabledEvents {
		_ = writeEventEnable(t.traceRoot, event, false)
	}
	if t.previousTracingOn != "" {
		_ = writeTracingFile(t.traceRoot, "tracing_on", strings.TrimSpace(t.previousTracingOn))
	}

	summary := restoreDeepTraceSummary{
		InstanceID:         t.instanceID,
		PID:                t.pid,
		TraceRoot:          t.traceRoot,
		TracePath:          tracePath,
		EnabledEvents:      t.enabledEvents,
		MissingEvents:      t.missingEvents,
		Marks:              t.marks,
		Samples:            t.samples,
		ProcessDeltas:      t.processDeltas(),
		IODeltas:           t.ioDeltas(),
		ThreadDeltaSummary: t.threadDeltaSummary(),
	}
	t.writeJSON("summary.json", summary)
	if t.log != nil {
		t.log.Info("restore deep trace finished", "instance_id", t.instanceID, "pid", t.pid, "dir", t.dir, "error", err)
	}
}

func (t *restoreDeepTrace) Dir() string {
	if t == nil {
		return ""
	}
	return t.dir
}

func (t *restoreDeepTrace) configureTracing() error {
	if data, err := os.ReadFile(filepath.Join(t.traceRoot, "tracing_on")); err == nil {
		t.previousTracingOn = string(data)
	}
	_ = writeTracingFile(t.traceRoot, "tracing_on", "0")
	_ = writeTracingFile(t.traceRoot, "trace", "0")
	_ = writeTracingFile(t.traceRoot, "buffer_size_kb", "16384")

	available, err := readAvailableTraceEvents(t.traceRoot)
	if err != nil {
		return err
	}
	for _, event := range restoreDeepTraceRequestedEvents() {
		if _, ok := available[event]; !ok {
			t.missingEvents = append(t.missingEvents, event)
			continue
		}
		if err := writeEventEnable(t.traceRoot, event, true); err != nil {
			t.missingEvents = append(t.missingEvents, event+"("+err.Error()+")")
			continue
		}
		t.enabledEvents = append(t.enabledEvents, event)
	}
	if len(t.enabledEvents) == 0 {
		return fmt.Errorf("no requested ftrace events could be enabled")
	}

	marker, err := os.OpenFile(filepath.Join(t.traceRoot, "trace_marker"), os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		t.traceMarker = marker
	}
	return writeTracingFile(t.traceRoot, "tracing_on", "1")
}

func findRestoreDeepTraceRoot() (string, error) {
	for _, path := range []string{"/sys/kernel/tracing", "/sys/kernel/debug/tracing"} {
		if info, err := os.Stat(filepath.Join(path, "available_events")); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("kernel tracing filesystem is not available")
}

func restoreDeepTraceRequestedEvents() []string {
	if raw := strings.TrimSpace(os.Getenv(restoreDeepTraceEventsEnv)); raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if event := strings.TrimSpace(part); event != "" {
				out = append(out, event)
			}
		}
		return out
	}
	return []string{
		"sched:sched_switch",
		"sched:sched_wakeup",
		"sched:sched_wakeup_new",
		"kvm:kvm_entry",
		"kvm:kvm_exit",
		"kvm:kvm_page_fault",
		"exceptions:page_fault_user",
		"exceptions:page_fault_kernel",
		"block:block_rq_issue",
		"block:block_rq_complete",
		"filemap:mm_filemap_add_to_page_cache",
		"filemap:mm_filemap_delete_from_page_cache",
		"filemap:filemap_add_to_page_cache",
		"filemap:filemap_delete_from_page_cache",
		"writeback:writeback_dirty_page",
	}
}

func readAvailableTraceEvents(traceRoot string) (map[string]struct{}, error) {
	data, err := os.ReadFile(filepath.Join(traceRoot, "available_events"))
	if err != nil {
		return nil, fmt.Errorf("read available ftrace events: %w", err)
	}
	out := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out[line] = struct{}{}
		}
	}
	return out, nil
}

func writeEventEnable(traceRoot, event string, enabled bool) error {
	system, name, ok := strings.Cut(event, ":")
	if !ok || system == "" || name == "" {
		return fmt.Errorf("invalid ftrace event %q", event)
	}
	value := "0"
	if enabled {
		value = "1"
	}
	return writeTracingFile(traceRoot, filepath.Join("events", system, name, "enable"), value)
}

func writeTracingFile(traceRoot, rel, value string) error {
	return os.WriteFile(filepath.Join(traceRoot, rel), []byte(value), 0644)
}

func readRestoreDeepTraceSample(stage string, pid int) restoreDeepTraceSample {
	sample := restoreDeepTraceSample{
		Stage: stage,
		Time:  time.Now().UTC(),
	}
	process, err := readRestoreDeepTraceThread(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		sample.ReadError = err.Error()
		return sample
	}
	sample.Process = process
	sample.IO = readRestoreDeepTraceIO(pid)
	sample.Threads = readRestoreDeepTraceThreads(pid)
	return sample
}

func readRestoreDeepTraceThreads(pid int) []restoreDeepTraceThread {
	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	threads := make([]restoreDeepTraceThread, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		thread, err := readRestoreDeepTraceThread(filepath.Join(taskDir, entry.Name(), "stat"))
		if err == nil {
			threads = append(threads, thread)
		}
	}
	sort.Slice(threads, func(i, j int) bool {
		return threads[i].TID < threads[j].TID
	})
	return threads
}

func readRestoreDeepTraceThread(path string) (restoreDeepTraceThread, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return restoreDeepTraceThread{}, err
	}
	raw := strings.TrimSpace(string(data))
	closeIdx := strings.LastIndex(raw, ")")
	openIdx := strings.Index(raw, "(")
	if openIdx < 0 || closeIdx < openIdx {
		return restoreDeepTraceThread{}, fmt.Errorf("invalid proc stat format for %s", path)
	}
	tid, _ := strconv.Atoi(strings.TrimSpace(raw[:openIdx]))
	comm := raw[openIdx+1 : closeIdx]
	fields := strings.Fields(strings.TrimSpace(raw[closeIdx+1:]))
	if len(fields) < 37 {
		return restoreDeepTraceThread{}, fmt.Errorf("short proc stat for %s", path)
	}
	return restoreDeepTraceThread{
		TID:        tid,
		Comm:       comm,
		State:      fields[0],
		MinFaults:  parseInt64Field(fields, 7),
		MajFaults:  parseInt64Field(fields, 9),
		UTimeTicks: parseInt64Field(fields, 11),
		STimeTicks: parseInt64Field(fields, 12),
		Threads:    parseInt64Field(fields, 17),
		Processor:  parseInt64Field(fields, 36),
	}, nil
}

func readRestoreDeepTraceIO(pid int) map[string]int64 {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "io"))
	if err != nil {
		return nil
	}
	out := make(map[string]int64)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err == nil {
			out[strings.TrimSpace(key)] = n
		}
	}
	return out
}

func parseInt64Field(fields []string, idx int) int64 {
	if idx < 0 || idx >= len(fields) {
		return 0
	}
	n, _ := strconv.ParseInt(fields[idx], 10, 64)
	return n
}

func (t *restoreDeepTrace) processDeltas() map[string]restoreDeepTraceThreadDelta {
	if t == nil || len(t.samples) < 2 {
		return nil
	}
	first := t.samples[0].Process
	out := make(map[string]restoreDeepTraceThreadDelta)
	for _, sample := range t.samples[1:] {
		out[sample.Stage] = deltaRestoreDeepTraceThread(first, sample.Process)
	}
	return out
}

func (t *restoreDeepTrace) ioDeltas() map[string]map[string]int64 {
	if t == nil || len(t.samples) < 2 || len(t.samples[0].IO) == 0 {
		return nil
	}
	first := t.samples[0].IO
	out := make(map[string]map[string]int64)
	for _, sample := range t.samples[1:] {
		if len(sample.IO) == 0 {
			continue
		}
		delta := make(map[string]int64)
		for key, value := range sample.IO {
			delta[key] = value - first[key]
		}
		out[sample.Stage] = delta
	}
	return out
}

func (t *restoreDeepTrace) threadDeltaSummary() map[string]restoreDeepTraceThreadDelta {
	if t == nil || len(t.samples) < 2 {
		return nil
	}
	first := threadsByID(t.samples[0].Threads)
	out := make(map[string]restoreDeepTraceThreadDelta)
	for _, sample := range t.samples[1:] {
		var sum restoreDeepTraceThreadDelta
		for _, thread := range sample.Threads {
			base, ok := first[thread.TID]
			if !ok {
				continue
			}
			delta := deltaRestoreDeepTraceThread(base, thread)
			sum.MinFaults += delta.MinFaults
			sum.MajFaults += delta.MajFaults
			sum.UTimeTicks += delta.UTimeTicks
			sum.STimeTicks += delta.STimeTicks
		}
		out[sample.Stage] = sum
	}
	return out
}

func threadsByID(threads []restoreDeepTraceThread) map[int]restoreDeepTraceThread {
	out := make(map[int]restoreDeepTraceThread, len(threads))
	for _, thread := range threads {
		out[thread.TID] = thread
	}
	return out
}

func deltaRestoreDeepTraceThread(start, end restoreDeepTraceThread) restoreDeepTraceThreadDelta {
	return restoreDeepTraceThreadDelta{
		MinFaults:  end.MinFaults - start.MinFaults,
		MajFaults:  end.MajFaults - start.MajFaults,
		UTimeTicks: end.UTimeTicks - start.UTimeTicks,
		STimeTicks: end.STimeTicks - start.STimeTicks,
	}
}

func (t *restoreDeepTrace) writeJSON(name string, value any) {
	if t == nil || t.dir == "" {
		return
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(t.dir, name), append(data, '\n'), 0644)
}

func sanitizeDeepTraceName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
	if name == "" {
		return "sample"
	}
	return name
}

func sanitizeTraceMarkerValue(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return value
}
