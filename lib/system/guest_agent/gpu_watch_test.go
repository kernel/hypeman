package main

import (
	"bytes"
	"io"
	"log"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGPUInitFailureMessage(t *testing.T) {
	msg, ok := gpuInitFailureMessage("3,1042,8462102,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n")
	assert.True(t, ok)
	assert.Equal(t, "NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)", msg)

	// Tuple values are driver-build-specific; the match must not depend on them.
	_, ok = gpuInitFailureMessage("3,1042,8462102,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x26:0xffff:1482)\n")
	assert.True(t, ok)

	// Any kernel log level matches; only the facility is load-bearing.
	_, ok = gpuInitFailureMessage("4,1044,8462120,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n")
	assert.True(t, ok)

	for _, record := range []string{
		"6,1041,8462100,-;NVRM: loading NVIDIA UNIX Open Kernel Module for x86_64\n",
		"6,1043,8462110,-;nvidia-gridd: RmInitAdapter failed mentioned in userspace\n",
		"no separator RmInitAdapter failed!\n",
		" continuation line of a multi-line record\n",
		// Userspace /dev/kmsg writes carry facility LOG_USER or higher — the
		// kernel coerces a facility-0 prefix to LOG_USER — so these records,
		// captured from a live 6.12 kernel by writing the driver's line into
		// /dev/kmsg from a root shell, must never convict:
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

// One report is emitted as several identical marker lines because kernel
// printk shares the serial console and can split a single write mid-marker
// — and a wedged VF guarantees printk traffic at report time. Any one
// intact copy convicts; the copies share one ts so they read as one report.
func TestEmitGPUInitFailureReportRepeatsMarkerLines(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	emitGPUInitFailureReport("NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, gpuReportRepeats)
	for i, line := range lines {
		assert.Contains(t, line, "HYPEMAN-GPU-INIT-FAILED ts=")
		assert.Contains(t, line, `nvrm="NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)"`)
		// Identical from the marker onward: same ts, one report.
		assert.Equal(t,
			lines[0][strings.Index(lines[0], "HYPEMAN"):],
			line[strings.Index(line, "HYPEMAN"):],
			"copy %d must be identical to the first", i)
	}
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

// kmsgConn mimics /dev/kmsg read(2) semantics: each read returns exactly one
// record, and a buffer smaller than the record fails with EINVAL without
// consuming it.
type kmsgConn struct {
	records []string
	pos     int
}

func (k *kmsgConn) Read(p []byte) (int, error) {
	if k.pos >= len(k.records) {
		return 0, io.EOF
	}
	rec := k.records[k.pos]
	if len(p) < len(rec) {
		return 0, syscall.EINVAL
	}
	k.pos++
	return copy(p, rec), nil
}

// A record larger than bufio's default 4 KiB buffer must not wedge the scan:
// /dev/kmsg rejects a short read with EINVAL without consuming the record,
// so an undersized buffer would replay into the same record on every reopen
// and never reach a failure line behind it.
func TestScanKmsgReadsOversizedRecords(t *testing.T) {
	oversized := "6,1,100,-;" + strings.Repeat("x", 5000) + "\n"
	failure := "3,2,200,-;NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)\n"

	var got []string
	require.NoError(t, scanKmsg(&kmsgConn{records: []string{oversized, failure}},
		func(msg string) { got = append(got, msg) }))
	assert.Len(t, got, 1)

	// A record beyond even the sized buffer surfaces the EINVAL instead of
	// ending the scan silently, so the watcher logs the wedge.
	huge := "6,3,300,-;" + strings.Repeat("x", kmsgRecordBufferBytes) + "\n"
	err := scanKmsg(&kmsgConn{records: []string{huge}}, func(string) {})
	assert.ErrorIs(t, err, syscall.EINVAL)
}
