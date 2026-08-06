//go:build linux

package hypervisor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var procDir = "/proc"

// ResolveProcessPID finds the process currently holding the listening Unix
// socket for the given hypervisor control path. confirmed reports whether the
// PID was found through socket ownership rather than its command line.
func ResolveProcessPID(socketPath string) (pid int, confirmed bool, err error) {
	socketRef, socketErr := socketRefForPath(socketPath)
	var refErr error
	if socketErr == nil {
		pid, refErr = pidBySocketRef(socketRef)
		if refErr == nil {
			return pid, true, nil
		}
	}

	if pid, cmdErr := pidByCmdline(socketPath); cmdErr == nil {
		return pid, false, nil
	}
	if refErr != nil {
		return 0, false, refErr
	}
	if socketErr != nil {
		return 0, false, socketErr
	}
	return 0, false, fmt.Errorf("resolve process pid for socket %s: no owning process found", socketPath)
}

func pidBySocketRef(socketRef string) (int, error) {
	procEntries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}

	var scanErr error
	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		fdEntries, err := os.ReadDir(filepath.Join(procDir, entry.Name(), "fd"))
		if err != nil {
			scanErr = err
			continue
		}
		for _, fdEntry := range fdEntries {
			target, err := os.Readlink(filepath.Join(procDir, entry.Name(), "fd", fdEntry.Name()))
			if err != nil {
				scanErr = err
				continue
			}
			if strings.TrimSpace(target) == socketRef {
				return pid, nil
			}
		}
	}

	if scanErr != nil {
		return 0, fmt.Errorf("resolve process pid for %s: inspect process fds: %w", socketRef, scanErr)
	}
	return 0, fmt.Errorf("resolve process pid for %s: no owning process found", socketRef)
}

func pidByCmdline(socketPath string) (int, error) {
	procEntries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}

	var scanErr error
	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		cmdline, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "cmdline"))
		if err != nil {
			scanErr = err
			continue
		}
		if len(cmdline) == 0 {
			continue
		}
		for _, arg := range strings.Split(string(cmdline), "\x00") {
			if arg == socketPath {
				return pid, nil
			}
		}
	}

	if scanErr != nil {
		return 0, fmt.Errorf("resolve process pid for socket %s: inspect process command lines: %w", socketPath, scanErr)
	}
	return 0, fmt.Errorf("resolve process pid for socket %s: no matching command line found", socketPath)
}

func socketRefForPath(socketPath string) (string, error) {
	file, err := os.Open(filepath.Join(procDir, "net", "unix"))
	if err != nil {
		return "", fmt.Errorf("open /proc/net/unix: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		if fields[0] == "Num" {
			continue
		}
		path := fields[len(fields)-1]
		if path != socketPath {
			continue
		}
		inode := fields[6]
		if inode == "" {
			break
		}
		return fmt.Sprintf("socket:[%s]", inode), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan /proc/net/unix: %w", err)
	}
	return "", fmt.Errorf("resolve process pid for socket %s: socket inode not found", socketPath)
}
