package main

import (
	"bytes"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
)

type wait4Func func(int, *syscall.WaitStatus, int, *syscall.Rusage) (int, error)

type knownChildPIDs struct {
	mu      sync.RWMutex
	pids    map[int]struct{}
	changed chan struct{}
}

func newKnownChildPIDs() *knownChildPIDs {
	return &knownChildPIDs{
		pids:    make(map[int]struct{}),
		changed: make(chan struct{}, 1),
	}
}

func (k *knownChildPIDs) add(pid int) {
	k.mu.Lock()
	k.pids[pid] = struct{}{}
	k.mu.Unlock()
}

func (k *knownChildPIDs) remove(pid int) {
	k.mu.Lock()
	_, existed := k.pids[pid]
	delete(k.pids, pid)
	k.mu.Unlock()
	if existed {
		select {
		case k.changed <- struct{}{}:
		default:
		}
	}
}

func (k *knownChildPIDs) contains(pid int) bool {
	k.mu.RLock()
	_, known := k.pids[pid]
	k.mu.RUnlock()
	return known
}

func startOrphanReaper(knownChildren *knownChildPIDs) func() {
	sigCh := make(chan os.Signal, 1)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	signal.Notify(sigCh, syscall.SIGCHLD)

	go func() {
		defer close(doneCh)
		reapAdoptedZombies("/proc", os.Getpid(), knownChildren, syscall.Wait4)
		for {
			select {
			case <-sigCh:
				reapAdoptedZombies("/proc", os.Getpid(), knownChildren, syscall.Wait4)
			case <-knownChildren.changed:
				reapAdoptedZombies("/proc", os.Getpid(), knownChildren, syscall.Wait4)
			case <-stopCh:
				return
			}
		}
	}()

	return func() {
		signal.Stop(sigCh)
		close(stopCh)
		<-doneCh
	}
}

func reapAdoptedZombies(procRoot string, parentPID int, knownChildren *knownChildPIDs, wait4 wait4Func) {
	for _, pid := range adoptedZombiePIDs(procRoot, parentPID, knownChildren) {
		var status syscall.WaitStatus
		_, _ = wait4(pid, &status, syscall.WNOHANG, nil)
	}
}

func adoptedZombiePIDs(procRoot string, parentPID int, knownChildren *knownChildPIDs) []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}

	pids := make([]int, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if knownChildren.contains(pid) {
			continue
		}

		stat, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		state, ppid, ok := parseProcStat(stat)
		if ok && state == 'Z' && ppid == parentPID {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

func parseProcStat(stat []byte) (byte, int, bool) {
	commEnd := bytes.LastIndexByte(stat, ')')
	if commEnd < 0 || commEnd+1 >= len(stat) {
		return 0, 0, false
	}
	fields := bytes.Fields(stat[commEnd+1:])
	if len(fields) < 2 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], ppid, true
}
