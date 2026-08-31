package qemu

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
