//go:build darwin

package vm_metrics

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParsePSCPUTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"12:47.29":      12*time.Minute + 47*time.Second + 290*time.Millisecond,
		"1:02:03.50":    time.Hour + 2*time.Minute + 3500*time.Millisecond,
		"2-01:00:00.00": 49 * time.Hour,
		"0:00.01":       10 * time.Millisecond,
	} {
		got, err := parsePSCPUTime(in)
		require.NoError(t, err, in)
		require.InDelta(t, float64(want), float64(got), float64(time.Millisecond), in)
	}
	for _, bad := range []string{"", "x:01", "1:2:3:4", "-1:00"} {
		_, err := parsePSCPUTime(bad)
		require.Error(t, err, bad)
	}
}

func TestParsePSSampleRejectsMalformedOutput(t *testing.T) {
	_, _, _, err := parsePSSample("0:01.00 100")
	require.Error(t, err)
	cpu, rss, vsz, err := parsePSSample("  0:01.50   2048  4096\n")
	require.NoError(t, err)
	require.Equal(t, 1500*time.Millisecond, cpu)
	require.EqualValues(t, 2048, rss)
	require.EqualValues(t, 4096, vsz)
}

// The live sample uses this test process, so it needs no VM and no privileges.
func TestReadProcStatSamplesLiveProcess(t *testing.T) {
	usec, err := ReadProcStat(os.Getpid())
	require.NoError(t, err)
	require.Greater(t, usec, uint64(0))
	rss, vms, err := ReadProcStatm(os.Getpid())
	require.NoError(t, err)
	require.Greater(t, rss, uint64(0))
	require.GreaterOrEqual(t, vms, rss)
}

func TestRelatedVMPIDsIgnoresOrdinaryHolders(t *testing.T) {
	require.Empty(t, relatedVMPIDs(""))
	path := t.TempDir() + "/held.raw"
	require.NoError(t, os.WriteFile(path, []byte("x"), 0600))
	f, err := os.Open(path) // held by this test process, which is not a VM process
	require.NoError(t, err)
	defer f.Close()
	require.Empty(t, relatedVMPIDs(path))
}
