package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProcStat(t *testing.T) {
	state, ppid, ok := parseProcStat([]byte("123 (chromium helper) Z 1 123 123 0"))
	require.True(t, ok)
	assert.Equal(t, byte('Z'), state)
	assert.Equal(t, 1, ppid)

	state, ppid, ok = parseProcStat([]byte("123 (name with ) parenthesis) S 42 123 123 0"))
	require.True(t, ok)
	assert.Equal(t, byte('S'), state)
	assert.Equal(t, 42, ppid)

	_, _, ok = parseProcStat([]byte("malformed"))
	assert.False(t, ok)
}

func TestAdoptedZombiePIDs(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStat(t, procRoot, 101, "chromium", 'Z', 1)
	writeProcStat(t, procRoot, 102, "known app", 'Z', 1)
	writeProcStat(t, procRoot, 103, "running child", 'S', 1)
	writeProcStat(t, procRoot, 104, "other parent", 'Z', 99)
	require.NoError(t, os.Mkdir(filepath.Join(procRoot, "105"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "105", "stat"), []byte("malformed"), 0o644))

	knownChildren := newKnownChildPIDs()
	knownChildren.add(102)
	got := adoptedZombiePIDs(procRoot, 1, knownChildren)
	assert.Equal(t, []int{101}, got)

	knownChildren.remove(102)
	got = adoptedZombiePIDs(procRoot, 1, knownChildren)
	assert.Equal(t, []int{101, 102}, got)
}

func TestKnownChildRemovalSignalsRescan(t *testing.T) {
	knownChildren := newKnownChildPIDs()
	knownChildren.add(102)
	knownChildren.remove(102)

	assert.False(t, knownChildren.contains(102))
	signaled := false
	select {
	case <-knownChildren.changed:
		signaled = true
	default:
	}
	assert.True(t, signaled)
}

func TestReapAdoptedZombies(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStat(t, procRoot, 201, "first", 'Z', 1)
	writeProcStat(t, procRoot, 202, "second", 'Z', 1)
	writeProcStat(t, procRoot, 203, "known", 'Z', 1)

	var reaped []int
	wait4 := func(pid int, _ *syscall.WaitStatus, options int, _ *syscall.Rusage) (int, error) {
		assert.Equal(t, syscall.WNOHANG, options)
		reaped = append(reaped, pid)
		return pid, nil
	}

	knownChildren := newKnownChildPIDs()
	knownChildren.add(203)
	reapAdoptedZombies(procRoot, 1, knownChildren, wait4)
	assert.Equal(t, []int{201, 202}, reaped)
}

func writeProcStat(t *testing.T, procRoot string, pid int, name string, state byte, ppid int) {
	t.Helper()
	dir := filepath.Join(procRoot, fmt.Sprintf("%d", pid))
	require.NoError(t, os.Mkdir(dir, 0o755))
	stat := fmt.Sprintf("%d (%s) %c %d 0 0 0", pid, name, state, ppid)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))
}
