package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ptpDevicePath = "/dev/ptp0"

	// Logged by the vmgenid driver when the hypervisor bumps the VM
	// generation ID, which happens on every snapshot restore.
	vmForkKmsgSignal = "crng reseeded due to virtual machine fork"

	driftCheckPeriod = 30 * time.Second
	maxClockDrift    = time.Second
)

// startClockKeeper keeps the guest realtime clock aligned with the host. The
// wall clock resumes from the snapshot's saved time after a standby restore
// and nothing else corrects it (the guest runs no NTP daemon, and VMGenID
// only reseeds the RNG). The keeper reads host time from the KVM PTP device
// and steps CLOCK_REALTIME whenever it drifts, checking periodically and
// immediately after a restore is detected via the vmgenid kmsg line.
func startClockKeeper() {
	ptp, err := os.Open(ptpDevicePath)
	if err != nil {
		log.Printf("[guest-agent] clock keeper disabled: %v", err)
		return
	}

	restored := make(chan struct{}, 1)
	go watchVMForkKmsg(restored)
	go runClockKeeper(ptp, restored)
}

func runClockKeeper(ptp *os.File, restored <-chan struct{}) {
	ticker := time.NewTicker(driftCheckPeriod)
	defer ticker.Stop()
	for {
		if err := syncClockFromPTP(ptp); err != nil {
			log.Printf("[guest-agent] clock sync failed: %v", err)
		}
		select {
		case <-ticker.C:
		case <-restored:
		}
	}
}

func syncClockFromPTP(ptp *os.File) error {
	// FD_TO_CLOCKID from linux/posix-timers.h
	clockID := int32((^int(ptp.Fd()) << 3) | 3)

	var hostTS unix.Timespec
	if err := unix.ClockGettime(clockID, &hostTS); err != nil {
		return fmt.Errorf("read %s: %w", ptpDevicePath, err)
	}
	var guestTS unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &guestTS); err != nil {
		return fmt.Errorf("read realtime clock: %w", err)
	}

	drift := time.Duration(hostTS.Nano() - guestTS.Nano())
	if drift.Abs() <= maxClockDrift {
		return nil
	}
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &hostTS); err != nil {
		return fmt.Errorf("set realtime clock: %w", err)
	}
	log.Printf("[guest-agent] stepped realtime clock by %s to %s",
		drift, time.Unix(0, hostTS.Nano()).UTC().Format(time.RFC3339Nano))
	return nil
}

func watchVMForkKmsg(restored chan<- struct{}) {
	f, err := os.Open("/dev/kmsg")
	if err != nil {
		log.Printf("[guest-agent] clock keeper kmsg watch disabled: %v", err)
		return
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		log.Printf("[guest-agent] warning: failed to seek /dev/kmsg to end: %v", err)
	}

	// Each read returns one log record. EPIPE means the buffer wrapped and
	// records were overwritten; the next read continues from the oldest
	// available record.
	buf := make([]byte, 8192)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if errors.Is(err, unix.EPIPE) {
				continue
			}
			log.Printf("[guest-agent] clock keeper kmsg watch stopped: %v", err)
			return
		}
		if strings.Contains(string(buf[:n]), vmForkKmsgSignal) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}
	}
}
