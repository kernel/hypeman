package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGPUInitFailureMessage(t *testing.T) {
	msg, ok := gpuInitFailureMessage("3,1042,8462102,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n")
	assert.True(t, ok)
	assert.Equal(t, "NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)", msg)

	_, ok = gpuInitFailureMessage("3,1042,8462102,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x26:0xffff:1482)\n")
	assert.True(t, ok)

	_, ok = gpuInitFailureMessage("4,1044,8462120,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n")
	assert.True(t, ok)

	for _, record := range []string{
		"6,1041,8462100,-;NVRM: loading NVIDIA UNIX Open Kernel Module for x86_64\n",
		"6,1043,8462110,-;nvidia-gridd: RmInitAdapter failed mentioned in userspace\n",
		"no separator RmInitAdapter failed!\n",
		" continuation line of a multi-line record\n",
		"12,307,4250363151,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n", // plain write
		"8,308,4250380620,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n",  // "<0>" prefix
		"9,310,5898419120,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n",  // "<1>" prefix
		"24,309,5898400480,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n", // "<24>" prefix (facility 3)
		"x,1,100,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n",           // malformed priority
	} {
		_, ok := gpuInitFailureMessage(record)
		assert.False(t, ok, "record %q must not match", record)
	}
}

func captureAgentLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(log.LstdFlags)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})
	return &buf
}

func TestProbeGPUInitMarksOKOnceDriverResponds(t *testing.T) {
	captureAgentLog(t)

	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "nvidia-smi"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir)

	reporter := &gpuInitReporter{}
	probeGPUInit(reporter)

	state, _ := reporter.state()
	assert.Equal(t, pb.GPUInitState_GPU_INIT_STATE_OK, state)
}

func TestProbeGPUInitRetriesAfterAttemptTimeout(t *testing.T) {
	captureAgentLog(t)
	attempts := 0

	reporter := &gpuInitReporter{}
	probeGPUInitUntil(reporter, time.Now().Add(time.Second), 10*time.Millisecond, 0, func() error {
		attempts++
		if attempts == 1 {
			return context.DeadlineExceeded
		}
		return nil
	})

	assert.Equal(t, 2, attempts)
	state, _ := reporter.state()
	assert.Equal(t, pb.GPUInitState_GPU_INIT_STATE_OK, state)
}

func TestProbeGPUInitSkipsWithoutNvidiaSMI(t *testing.T) {
	buf := captureAgentLog(t)
	t.Setenv("PATH", t.TempDir())

	probeGPUInit(&gpuInitReporter{})

	assert.Empty(t, buf.String())
}

func TestGPUInitReporterMakesSuccessTerminal(t *testing.T) {
	captureAgentLog(t)
	reporter := &gpuInitReporter{}
	reporter.reportFailure("NVRM: GPU 0000:00:03.0: RmInitAdapter failed!")
	reporter.reportSuccess()
	reporter.reportFailure("NVRM: GPU 0000:00:03.0: RmInitAdapter failed!")

	state, msg := reporter.state()
	assert.Equal(t, pb.GPUInitState_GPU_INIT_STATE_OK, state)
	assert.Empty(t, msg)
}

func TestGPUInitReporterState(t *testing.T) {
	captureAgentLog(t)

	server := &guestServer{}
	resp, err := server.GetGPUInitStatus(context.Background(), &pb.GetGPUInitStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, pb.GPUInitState_GPU_INIT_STATE_UNKNOWN, resp.State, "a host without an NVIDIA device has no reporter")

	reporter := &gpuInitReporter{}
	server = &guestServer{gpuReporter: reporter}
	state, _ := reporter.state()
	assert.Equal(t, pb.GPUInitState_GPU_INIT_STATE_UNKNOWN, state)

	reporter.reportFailure("NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)")
	reporter.reportFailure("NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x26:0xffff:1482)")
	resp, err = server.GetGPUInitStatus(context.Background(), &pb.GetGPUInitStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, pb.GPUInitState_GPU_INIT_STATE_FAILED, resp.State)
	assert.Equal(t, "NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x26:0xffff:1482)", resp.FailureMessage, "the latest failure line wins")

	reporter.reportSuccess()
	resp, err = server.GetGPUInitStatus(context.Background(), &pb.GetGPUInitStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, pb.GPUInitState_GPU_INIT_STATE_OK, resp.State)
	assert.Empty(t, resp.FailureMessage)
}

func TestRunGPUProbeAttemptReturnsOnTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nvidia-smi")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755))

	start := time.Now()
	err := runGPUProbeAttempt(path, 10*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
}

func TestScanKmsgReportsEachFailureRecord(t *testing.T) {
	records := strings.Join([]string{
		"6,1,100,-;booting",
		"3,2,200,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)",
		"6,3,300,-;unrelated",
		"3,4,400,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)",
	}, "\n") + "\n"

	var got []string
	require.NoError(t, scanKmsg(strings.NewReader(records), func(msg string) { got = append(got, msg) }))
	assert.Len(t, got, 2)
}

// kmsgOverrun marks a read that fails with EPIPE, as /dev/kmsg does when
// records are overwritten while being read.
const kmsgOverrun = "\x00overrun"

type kmsgConn struct {
	records []string
	pos     int
}

func (k *kmsgConn) Read(p []byte) (int, error) {
	if k.pos >= len(k.records) {
		return 0, io.EOF
	}
	rec := k.records[k.pos]
	if rec == kmsgOverrun {
		k.pos++
		return 0, syscall.EPIPE
	}
	if len(p) < len(rec) {
		return 0, syscall.EINVAL
	}
	k.pos++
	return copy(p, rec), nil
}

func TestScanKmsgReadsOversizedRecords(t *testing.T) {
	oversized := "6,1,100,-;" + strings.Repeat("x", 5000) + "\n"
	failure := "3,2,200,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n"

	var got []string
	require.NoError(t, scanKmsg(&kmsgConn{records: []string{oversized, failure}},
		func(msg string) { got = append(got, msg) }))
	assert.Len(t, got, 1)

	huge := "6,3,300,-;" + strings.Repeat("x", kmsgRecordBufferBytes) + "\n"
	err := scanKmsg(&kmsgConn{records: []string{huge}}, func(string) {})
	assert.ErrorIs(t, err, syscall.EINVAL)
}

func TestScanKmsgContinuesAfterOverrun(t *testing.T) {
	failure := "3,2,200,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n"

	var got []string
	require.NoError(t, scanKmsg(&kmsgConn{records: []string{kmsgOverrun, failure}},
		func(msg string) { got = append(got, msg) }))
	assert.Len(t, got, 1, "records after an overrun must still be scanned on the same fd")
}
