package qemu

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/lib/logger"
)

// logVsockCIDConflict is a deliberate, log-only diagnostic used to confirm the
// vsock guest-CID teardown/handoff race hypothesis on shared CI runners. It is
// NOT part of normal control flow: it never fails the caller and only emits
// logs (via the package logger so they land in CI test output).
//
// It activates only when qemuOutput looks like a vsock guest-CID collision
// ("vhost-vsock: unable to set guest cid: Address already in use"). When it
// does, it logs the attempted CID and scans the host's /proc to identify which
// process is currently holding that CID, plus a summary of all live VMM
// processes (firecracker/cloud-hypervisor don't carry the CID in argv, so a
// holder may not be a qemu process we can match by CID).
func logVsockCIDConflict(ctx context.Context, cid int64, qemuOutput string) {
	lower := strings.ToLower(qemuOutput)
	if !strings.Contains(lower, "guest cid") || !strings.Contains(lower, "in use") {
		return
	}

	log := logger.FromContext(ctx)
	log.WarnContext(ctx, "vsock guest CID conflict detected; scanning host for holder (diagnostic)",
		"attempted_cid", cid)

	// 1. Find qemu processes whose argv references this specific guest-cid.
	cidArg := "guest-cid=" + strconv.FormatInt(cid, 10)
	for _, proc := range scanProcesses() {
		for _, arg := range proc.argv {
			if strings.Contains(arg, cidArg) {
				log.WarnContext(ctx, "vsock CID holder found (qemu argv matches guest-cid)",
					"attempted_cid", cid,
					"holder_pid", proc.pid,
					"holder_ppid", proc.ppid,
					"holder_orphaned", proc.ppid == 1,
					"holder_tmp_dir", proc.tmpDir)
				break
			}
		}
	}

	// 2. Summarize all live VMM processes. firecracker/cloud-hypervisor do not
	// embed the CID in argv, so this lets us correlate a holder even when the
	// match above finds nothing.
	for _, proc := range scanProcesses() {
		if !isVMMComm(proc.comm) {
			continue
		}
		log.WarnContext(ctx, "live VMM process (diagnostic summary)",
			"comm", proc.comm,
			"pid", proc.pid,
			"ppid", proc.ppid,
			"orphaned", proc.ppid == 1,
			"tmp_dir", proc.tmpDir)
	}
}

type procInfo struct {
	pid    int
	ppid   int
	comm   string
	argv   []string
	tmpDir string
}

// scanProcesses reads /proc and returns one entry per live process. It is
// defensive: any unreadable proc entry is skipped. On non-Linux hosts /proc is
// absent and this returns an empty slice.
func scanProcesses() []procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var procs []procInfo
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a PID directory
		}
		procDir := filepath.Join("/proc", entry.Name())

		argv := readCmdline(filepath.Join(procDir, "cmdline"))
		if len(argv) == 0 {
			continue // kernel thread or exited
		}

		procs = append(procs, procInfo{
			pid:    pid,
			ppid:   readPPID(filepath.Join(procDir, "stat")),
			comm:   filepath.Base(argv[0]),
			argv:   argv,
			tmpDir: extractTmpDir(argv),
		})
	}
	return procs
}

// readCmdline reads a /proc/<pid>/cmdline file (NUL-separated argv).
func readCmdline(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	argv := parts[:0]
	for _, p := range parts {
		if p != "" {
			argv = append(argv, p)
		}
	}
	return argv
}

// readPPID parses the parent PID from /proc/<pid>/stat. The comm field is
// parenthesized and may contain spaces/parens, so we parse after the final ')'.
func readPPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := string(data)
	close := strings.LastIndex(s, ")")
	if close < 0 || close+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[close+2:])
	// After "(comm) ": field[0]=state, field[1]=ppid.
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// extractTmpDir returns the first /tmp or /ci path referenced in argv (the test
// scratch dir), to identify which test run owns the process.
func extractTmpDir(argv []string) string {
	for _, arg := range argv {
		for _, field := range strings.Split(arg, "=") {
			if strings.HasPrefix(field, "/tmp/") || strings.HasPrefix(field, "/ci/") ||
				field == "/tmp" || field == "/ci" {
				return field
			}
		}
	}
	return ""
}

// guestCIDFromArgs extracts the vhost-vsock guest-cid from a built QEMU argv,
// matching the "vhost-vsock-pci,guest-cid=%d" device argument. Returns 0 if not
// found.
func guestCIDFromArgs(args []string) int64 {
	const marker = "guest-cid="
	for _, arg := range args {
		idx := strings.Index(arg, marker)
		if idx < 0 {
			continue
		}
		rest := arg[idx+len(marker):]
		// guest-cid value may be followed by another comma-separated option.
		if comma := strings.IndexByte(rest, ','); comma >= 0 {
			rest = rest[:comma]
		}
		if cid, err := strconv.ParseInt(rest, 10, 64); err == nil {
			return cid
		}
	}
	return 0
}

// isVMMComm reports whether a process command name is a known VMM binary.
func isVMMComm(comm string) bool {
	return strings.HasPrefix(comm, "qemu-system") ||
		strings.Contains(comm, "firecracker") ||
		strings.Contains(comm, "cloud-hypervisor")
}
