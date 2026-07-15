package main

import (
	"bytes"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
)

type wait4Func func(int, *syscall.WaitStatus, int, *syscall.Rusage) (int, error)

func startOrphanReaper(knownChildren map[int]struct{}) func() {
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

func reapAdoptedZombies(procRoot string, parentPID int, knownChildren map[int]struct{}, wait4 wait4Func) {
	for _, pid := range adoptedZombiePIDs(procRoot, parentPID, knownChildren) {
		var status syscall.WaitStatus
		_, _ = wait4(pid, &status, syscall.WNOHANG, nil)
	}
}

func adoptedZombiePIDs(procRoot string, parentPID int, knownChildren map[int]struct{}) []int {
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
		if _, known := knownChildren[pid]; known {
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
