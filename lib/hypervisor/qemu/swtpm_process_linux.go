package qemu

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func discoverSWTPMProcess(config *hypervisor.TPMConfig) (swtpmProcessRecord, bool, error) {
	pid, err := hypervisor.ResolveProcessPID(config.SocketPath)
	if err == nil {
		matches, err := swtpmProcessMatchesConfig(pid, config)
		if err != nil {
			return swtpmProcessRecord{}, false, err
		}
		if !matches {
			return swtpmProcessRecord{}, false, fmt.Errorf("process %d owns %s but is not the expected swtpm", pid, config.SocketPath)
		}
		return swtpmProcessRecordForPID(pid)
	}
	if !errors.Is(err, hypervisor.ErrNoOwningProcess) {
		return swtpmProcessRecord{}, false, err
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return swtpmProcessRecord{}, false, err
	}
	var found swtpmProcessRecord
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		matches, err := swtpmProcessMatchesConfig(pid, config)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return swtpmProcessRecord{}, false, err
		}
		if !matches {
			continue
		}
		record, alive, err := swtpmProcessRecordForPID(pid)
		if err != nil {
			return swtpmProcessRecord{}, false, err
		}
		if !alive {
			continue
		}
		if found.pid != 0 {
			return swtpmProcessRecord{}, false, fmt.Errorf("multiple swtpm processes use %s", config.SocketPath)
		}
		found = record
	}
	return found, found.pid != 0, nil
}

func swtpmProcessMatchesConfig(pid int, config *hypervisor.TPMConfig) (bool, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false, err
	}
	args := bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0})
	if len(args) == 0 || filepath.Base(string(args[0])) != "swtpm" {
		return false, nil
	}
	stateArg := "dir=" + config.StateDir
	controlArg := "type=unixio,path=" + config.SocketPath
	var hasState, hasControl bool
	for _, arg := range args[1:] {
		hasState = hasState || string(arg) == stateArg
		hasControl = hasControl || string(arg) == controlArg
	}
	return hasState && hasControl, nil
}

func swtpmProcessRecordForPID(pid int) (swtpmProcessRecord, bool, error) {
	identity, alive, err := swtpmProcessIdentity(pid)
	return swtpmProcessRecord{pid: pid, identity: identity}, alive, err
}

func swtpmProcessIdentity(pid int) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat"))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	// The command name is parenthesized and may contain spaces. Fields after
	// the final ')' begin at stat field 3; process start time is field 22.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return "", false, fmt.Errorf("invalid /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", false, fmt.Errorf("incomplete /proc/%d/stat", pid)
	}
	return fields[19], true, nil
}
