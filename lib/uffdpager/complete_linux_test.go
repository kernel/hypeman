//go:build linux

package uffdpager

import (
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
