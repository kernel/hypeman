package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
)

const (
	gpuInitFailedSentinelPrefix = "HYPEMAN-GPU-INIT-FAILED"
	gpuInitOKSentinelPrefix     = "HYPEMAN-GPU-INIT-OK"

	kmsgPath          = "/dev/kmsg"
	nvidiaPCIVendorID = "0x10de"

	gpuProbeInterval = 15 * time.Second
	gpuProbeTimeout  = 10 * time.Minute
	// nvidia-smi can hang indefinitely on a wedged VF; without a per-attempt
	// bound the overall probe deadline is never reached.
	gpuProbeAttemptTimeout = 30 * time.Second

	gpuReportThrottle = 30 * time.Second

	// Concurrent guest console output can corrupt a serial line. The host accepts
	// only complete standalone markers and deduplicates repeats per assignment, so
	// repetition is safe and improves the odds that one marker arrives intact.
	gpuReportRepeats = 3

	kmsgReopenDelay = 5 * time.Second

	// /dev/kmsg returns EINVAL without consuming records larger than this buffer.
	kmsgRecordBufferBytes = 8192

	kmsgOpenRetryDelay = time.Minute
)

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

type gpuInitReporter struct {
	mu          sync.Mutex
	succeeded   bool
	failed      bool
	lastFailure time.Time
}

func (r *gpuInitReporter) reportFailure(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.succeeded {
		return
	}
	r.failed = true
	if time.Since(r.lastFailure) < gpuReportThrottle {
		return
	}
	r.lastFailure = time.Now()
	emitGPUInitFailureReport(msg)
}

func (r *gpuInitReporter) state() pb.GPUInitState {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.succeeded:
		return pb.GPUInitState_GPU_INIT_STATE_OK
	case r.failed:
		return pb.GPUInitState_GPU_INIT_STATE_FAILED
	default:
		return pb.GPUInitState_GPU_INIT_STATE_UNKNOWN
	}
}

// GetGPUInitStatus lets the host corroborate serial-console GPU markers, which
// share the console with workload output and are forgeable on their own.
func (s *guestServer) GetGPUInitStatus(context.Context, *pb.GetGPUInitStatusRequest) (*pb.GetGPUInitStatusResponse, error) {
	state := pb.GPUInitState_GPU_INIT_STATE_UNKNOWN
	if s.gpuReporter != nil {
		state = s.gpuReporter.state()
	}
	return &pb.GetGPUInitStatusResponse{State: state}, nil
}

func (r *gpuInitReporter) reportSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.succeeded {
		return
	}
	r.succeeded = true
	emitGPUInitOKReport()
}

func watchGPUInitFailure(reporter *gpuInitReporter) {
	firstOpen := true
	for {
		f, err := os.Open(kmsgPath)
		if err != nil {
			log.Printf("[guest-agent] cannot open %s for GPU init watch (retrying): %v", kmsgPath, err)
			time.Sleep(kmsgOpenRetryDelay)
			continue
		}
		// A reopened fd restarts at the oldest record; skip history already
		// scanned so a stale failure line is not reported again.
		if !firstOpen {
			if _, err := f.Seek(0, io.SeekEnd); err != nil {
				log.Printf("[guest-agent] cannot seek %s to end (may re-report old records): %v", kmsgPath, err)
			}
		}
		firstOpen = false
		err = scanKmsg(f, reporter.reportFailure)
		_ = f.Close()
		if err != nil {
			log.Printf("[guest-agent] GPU init watch read %s failed (reopening): %v", kmsgPath, err)
		}
		time.Sleep(kmsgReopenDelay)
	}
}

func emitGPUInitFailureReport(msg string) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	for range gpuReportRepeats {
		log.Printf("[guest-agent] %s ts=%s nvrm=%q", gpuInitFailedSentinelPrefix, ts, msg)
	}
}

// probeGPUInit reports successful guest driver init. Opening the device via
// nvidia-smi runs RmInitAdapter, so on a wedged VF the probe itself produces
// the failure line in kmsg without waiting for the workload to touch the GPU.
func probeGPUInit(reporter *gpuInitReporter) {
	nvidiaSMI, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return
	}
	probeGPUInitUntil(reporter, time.Now().Add(gpuProbeTimeout), gpuProbeAttemptTimeout, gpuProbeInterval, func() error {
		return runGPUProbeAttempt(nvidiaSMI, gpuProbeAttemptTimeout)
	})
}

func probeGPUInitUntil(reporter *gpuInitReporter, deadline time.Time, attemptTimeout, interval time.Duration, attempt func() error) {
	for {
		err := attempt()
		if err == nil {
			reporter.reportSuccess()
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[guest-agent] GPU init probe attempt timed out after %s", attemptTimeout)
		}
		if time.Now().After(deadline) {
			log.Printf("[guest-agent] GPU init probe gave up after %s", gpuProbeTimeout)
			return
		}
		time.Sleep(interval)
	}
}

func runGPUProbeAttempt(nvidiaSMI string, timeout time.Duration) error {
	cmd := exec.Command(nvidiaSMI, "-L")
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = cmd.Process.Kill()
		// Wait for the process to be reaped so at most one nvidia-smi attempt is
		// ever outstanding; a probe stuck in an uninterruptible ioctl blocks here
		// instead of accumulating processes, and the kmsg watcher still reports
		// the underlying init failure.
		<-done
		return context.DeadlineExceeded
	}
}

func emitGPUInitOKReport() {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	for range gpuReportRepeats {
		log.Printf("[guest-agent] %s ts=%s", gpuInitOKSentinelPrefix, ts)
	}
}

func scanKmsg(r io.Reader, report func(msg string)) error {
	reader := bufio.NewReaderSize(r, kmsgRecordBufferBytes)
	for {
		record, err := reader.ReadString('\n')
		if msg, ok := gpuInitFailureMessage(record); ok {
			report(msg)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			// EPIPE means records were overwritten while reading; the fd
			// continues at the next available record.
			if errors.Is(err, syscall.EPIPE) {
				continue
			}
			return err
		}
	}
}

// Only kernel-facility records match; userspace /dev/kmsg writes use LOG_USER.
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
