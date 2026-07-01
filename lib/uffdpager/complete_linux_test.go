//go:build linux

package uffdpager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestUffdioUnregisterValue guards the hand-computed ioctl number against typos
// by recomputing it from the asm-generic _IOR encoding used on the amd64/arm64
// hosts Hypeman runs on (the same encoding uffdioCopy relies on).
func TestUffdioUnregisterValue(t *testing.T) {
	const (
		iocRead      = 2
		dirShift     = 30
		sizeShift    = 16
		typeShift    = 8
		uffdioMagic  = 0xAA
		unregisterNr = 1
		rangeSize    = 16 // sizeof(struct uffdio_range)
	)
	want := uintptr(iocRead<<dirShift | rangeSize<<sizeShift | uffdioMagic<<typeShift | unregisterNr)
	if uintptr(uffdioUnregister) != want {
		t.Fatalf("uffdioUnregister = %#x, want %#x", uintptr(uffdioUnregister), want)
	}
}

func TestWakePipeDrains(t *testing.T) {
	r, w, err := newWakePipe()
	if err != nil {
		t.Fatalf("newWakePipe: %v", err)
	}
	defer unix.Close(r)
	defer unix.Close(w)

	// Draining an empty non-blocking pipe must return promptly.
	drainWake(r)

	if _, err := unix.Write(w, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write wake: %v", err)
	}
	drainWake(r)

	var b [1]byte
	if n, _ := unix.Read(r, b[:]); n > 0 {
		t.Fatalf("expected wake pipe drained, read %d bytes", n)
	}
}

// TestCompleteAbandonedContextAbortsBeforeUnregister guards the invariant that
// an abandoned /complete (caller timed out or disconnected) never reaches
// unregister: the populate aborts and the session must keep serving.
func TestCompleteAbandonedContextAbortsBeforeUnregister(t *testing.T) {
	backing, err := os.CreateTemp(t.TempDir(), "backing")
	if err != nil {
		t.Fatalf("create backing file: %v", err)
	}
	defer backing.Close()
	if err := backing.Truncate(8192); err != nil {
		t.Fatalf("truncate backing file: %v", err)
	}

	sess := &session{
		id:          "test",
		server:      &server{},
		backingFile: backing,
		uffdFD:      -1,
		mappings: []guestRegionUffdMapping{
			{BaseHostVirtAddr: 0x7f00_0000_0000, Size: 8192, Offset: 0, PageSize: 4096},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sess.completeAndUnregister(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("completeAndUnregister = %v, want context.Canceled", err)
	}
}

// TestWakeAfterCloseNoops exercises wake() racing close(): once the session is
// closed, wakeW is -1 under wakeMu, so wake must never write to the closed (and
// possibly recycled) fd. Run with -race to catch regressions.
func TestWakeAfterCloseNoops(t *testing.T) {
	r, w, err := newWakePipe()
	if err != nil {
		t.Fatalf("newWakePipe: %v", err)
	}
	sess := &session{
		id:         "test",
		done:       make(chan struct{}),
		socketPath: filepath.Join(t.TempDir(), "s.sock"),
		uffdFD:     -1,
		wakeR:      r,
		wakeW:      w,
	}

	woke := make(chan struct{})
	go func() {
		defer close(woke)
		for i := 0; i < 1000; i++ {
			sess.wake()
		}
	}()
	sess.close()
	<-woke

	sess.wakeMu.Lock()
	defer sess.wakeMu.Unlock()
	if sess.wakeW != -1 {
		t.Fatalf("wakeW = %d, want -1 after close", sess.wakeW)
	}
}
