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
	kmsgPath          = "/dev/kmsg"
	nvidiaPCIVendorID = "0x10de"

	gpuProbeInterval    = 15 * time.Second
	gpuProbeRetryWindow = 10 * time.Minute
	// A slow attempt is killed after this delay. If it is stuck in
	// uninterruptible I/O, the probe waits for it instead of starting another.
	gpuProbeAttemptKillAfter = 30 * time.Second

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
	mu             sync.Mutex
	succeeded      bool
	failed         bool
	failureMessage string
}

func (r *gpuInitReporter) reportFailure(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.succeeded {
		return
	}
	if !r.failed {
		log.Printf("[guest-agent] GPU init failure detected: %s", msg)
	}
	r.failed = true
	r.failureMessage = msg
}

func (r *gpuInitReporter) state() (pb.GPUInitState, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.succeeded:
		return pb.GPUInitState_GPU_INIT_STATE_OK, ""
	case r.failed:
		return pb.GPUInitState_GPU_INIT_STATE_FAILED, r.failureMessage
	default:
		return pb.GPUInitState_GPU_INIT_STATE_UNKNOWN, ""
	}
}

// GetGPUInitStatus reports the GPU driver init state to the host sentinel.
// The serial console is shared with workload output, so this vsock channel is
// the only signal the host trusts.
func (s *guestServer) GetGPUInitStatus(context.Context, *pb.GetGPUInitStatusRequest) (*pb.GetGPUInitStatusResponse, error) {
	state := pb.GPUInitState_GPU_INIT_STATE_UNKNOWN
	msg := ""
	if s.gpuReporter != nil {
		state, msg = s.gpuReporter.state()
	}
	return &pb.GetGPUInitStatusResponse{State: state, FailureMessage: msg}, nil
}

func (r *gpuInitReporter) reportSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.succeeded {
		return
	}
	r.succeeded = true
	log.Printf("[guest-agent] GPU driver initialized")
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

// probeGPUInit reports successful guest driver init. Opening the device via
// nvidia-smi runs RmInitAdapter, so on a wedged VF the probe itself produces
// the failure line in kmsg without waiting for the workload to touch the GPU.
func probeGPUInit(reporter *gpuInitReporter) {
	nvidiaSMI, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return
	}
	probeGPUInitUntil(reporter, time.Now().Add(gpuProbeRetryWindow), gpuProbeAttemptKillAfter, gpuProbeInterval, func() error {
		return runGPUProbeAttempt(nvidiaSMI, gpuProbeAttemptKillAfter)
	})
}

func probeGPUInitUntil(reporter *gpuInitReporter, deadline time.Time, attemptKillAfter, interval time.Duration, attempt func() error) {
	for {
		err := attempt()
		if err == nil {
			reporter.reportSuccess()
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[guest-agent] GPU init probe attempt exceeded %s and was killed", attemptKillAfter)
		}
		if time.Now().After(deadline) {
			log.Printf("[guest-agent] GPU init probe gave up after %s", gpuProbeRetryWindow)
			return
		}
		time.Sleep(interval)
	}
}

func runGPUProbeAttempt(nvidiaSMI string, killAfter time.Duration) error {
	cmd := exec.Command(nvidiaSMI, "-L")
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(killAfter)
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
