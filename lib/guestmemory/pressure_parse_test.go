package guestmemory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLinuxMeminfo(t *testing.T) {
	t.Parallel()

	total, available, err := parseLinuxMeminfo(`
MemTotal:       16384256 kB
MemFree:         1122334 kB
MemAvailable:    9988776 kB
Buffers:          123456 kB
`)
	require.NoError(t, err)
	assert.Equal(t, int64(16384256*1024), total)
	assert.Equal(t, int64(9988776*1024), available)
}

func TestParseLinuxMeminfoRequiresTotalAndAvailable(t *testing.T) {
	t.Parallel()

	_, _, err := parseLinuxMeminfo("MemTotal: 1024 kB\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing memory totals")
}

func TestParseLinuxPSIAboveThresholdIsStressed(t *testing.T) {
	t.Parallel()

	stressed, err := parseLinuxPSI(`
some avg10=0.25 avg60=0.12 avg300=0.05 total=12345
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
`)
	require.NoError(t, err)
	assert.True(t, stressed)
}

func TestParseLinuxPSIBelowThresholdIsHealthy(t *testing.T) {
	t.Parallel()

	stressed, err := parseLinuxPSI(`
some avg10=0.09 avg60=0.01 avg300=0.10 total=12345
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
`)
	require.NoError(t, err)
	assert.False(t, stressed)
}

func TestParseLinuxPSIZeroAvg10IsHealthy(t *testing.T) {
	t.Parallel()

	stressed, err := parseLinuxPSI(`
some avg10=0.00 avg60=0.01 avg300=0.10 total=12345
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
`)
	require.NoError(t, err)
	assert.False(t, stressed)
}

func TestParseDarwinVMStatOutput(t *testing.T) {
	t.Parallel()

	total, available, err := parseDarwinVMStatOutput(`
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               100.
Pages active:                           10000.
Pages inactive:                          2000.
Pages speculative:                        50.
`, "17179869184\n")
	require.NoError(t, err)
	assert.Equal(t, int64(17179869184), total)
	assert.Equal(t, int64(150*16384), available)
}

func TestParseDarwinPageCountRejectsMalformedLine(t *testing.T) {
	t.Parallel()

	_, err := parseDarwinPageCount("Pages free 100")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse vm_stat line")
}

func TestParseDarwinMemoryPressureOutput(t *testing.T) {
	t.Parallel()

	stressed, err := parseDarwinMemoryPressureOutput(`
The system has 1234 pages wired down.
System-wide memory free percentage: 8%
`)
	require.NoError(t, err)
	assert.True(t, stressed)
}

func TestParseDarwinMemoryPressureOutputHealthy(t *testing.T) {
	t.Parallel()

	stressed, err := parseDarwinMemoryPressureOutput(`
The system has 1234 pages wired down.
System-wide memory free percentage: 21%
`)
	require.NoError(t, err)
	assert.False(t, stressed)
}
