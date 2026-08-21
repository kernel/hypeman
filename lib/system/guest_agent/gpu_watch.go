package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// gpuInitFailedSentinelPrefix is the guest-to-host report of a failed
	// NVIDIA driver init. It rides the same channel as the other HYPEMAN-*
	// markers: agent log output reaches the serial console, which the host
	// persists per instance and scans. The host quarantines the vGPU VF on
	// this marker, so it is only ever emitted for a kernel-log line the
	// driver itself produced.
	gpuInitFailedSentinelPrefix = "HYPEMAN-GPU-INIT-FAILED"

	kmsgPath          = "/dev/kmsg"
	nvidiaPCIVendorID = "0x10de"

	// gpuReportThrottle bounds marker emission. The driver retries init
	// every ~20s on a wedged VF, and reopening /dev/kmsg replays the ring
	// buffer, so without a floor a long-lived broken guest would spam the
	// console.
	gpuReportThrottle = 30 * time.Second

	kmsgReopenDelay = 5 * time.Second

	// kmsgOpenRetryDelay paces reopen attempts after a failed /dev/kmsg
	// open, so a guest where the open fails does not silently lose wedge
	// detection for its whole lifetime.
	kmsgOpenRetryDelay = time.Minute
)

// hasNVIDIADevice reports whether any PCI function belongs to NVIDIA. A vGPU
// guest always enumerates its VF, even on a wedged slot, so a guest with no
// NVIDIA function never needs the watcher.
func hasNVIDIADevice() bool {
	vendors, _ := filepath.Glob("/sys/bus/pci/devices/*/vendor")
	for _, path := range vendors {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == nvidiaPCIVendorID {
			return true
		}
	}
	return false
}

// watchGPUInitFailure tails the guest kernel log for the NVIDIA driver's
// RmInitAdapter failure and reports each occurrence with a
// HYPEMAN-GPU-INIT-FAILED marker. Opening /dev/kmsg replays the ring buffer
// from the start, so failures that predate the agent are reported too. Reads
// error with EPIPE when the ring overwrites the read position; reopen and
// resume.
func watchGPUInitFailure() {
	var lastReport time.Time
	for {
		f, err := os.Open(kmsgPath)
		if err != nil {
			log.Printf("[guest-agent] cannot open %s for GPU init watch (retrying): %v", kmsgPath, err)
			time.Sleep(kmsgOpenRetryDelay)
			continue
		}
		scanKmsg(f, func(msg string) {
			if time.Since(lastReport) < gpuReportThrottle {
				return
			}
			lastReport = time.Now()
			log.Printf("[guest-agent] %s ts=%s nvrm=%q", gpuInitFailedSentinelPrefix, time.Now().UTC().Format(time.RFC3339Nano), msg)
		})
		_ = f.Close()
		time.Sleep(kmsgReopenDelay)
	}
}

// scanKmsg reads /dev/kmsg records from r and calls report for each GPU
// init-failure message.
func scanKmsg(r io.Reader, report func(msg string)) {
	reader := bufio.NewReader(r)
	for {
		record, err := reader.ReadString('\n')
		if msg, ok := gpuInitFailureMessage(record); ok {
			report(msg)
		}
		if err != nil {
			return
		}
	}
}

// gpuInitFailureMessage extracts the message from a /dev/kmsg record
// ("<priority>,<seq>,<ts>,<flags>;<message>") and reports whether it is the
// NVIDIA driver's init-failure line. Only kernel records match: the priority
// field encodes facility*8+level, kernel printk is always facility 0, and
// the kernel assigns userspace /dev/kmsg writers LOG_USER or higher (a
// facility-0 prefix from userspace is coerced to LOG_USER), so a process
// inside the guest cannot forge a matching record. The full line shape is
// required — the trailing (stage:status:line) tuple is driver-build-specific
// and unsafe to match.
func gpuInitFailureMessage(record string) (string, bool) {
	prefix, msg, found := strings.Cut(record, ";")
	if !found {
		return "", false
	}
	priority, _, found := strings.Cut(prefix, ",")
	if !found {
		return "", false
	}
	value, err := strconv.ParseUint(priority, 10, 32)
	if err != nil || value>>3 != 0 {
		return "", false
	}
	msg = strings.TrimSpace(msg)
	if !strings.HasPrefix(msg, "NVRM:") || !strings.Contains(msg, "RmInitAdapter failed!") {
		return "", false
	}
	return msg, true
}
