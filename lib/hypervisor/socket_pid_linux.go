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

// ResolveProcessPID finds the process currently holding the listening Unix
// socket for the given hypervisor control path.
func ResolveProcessPID(socketPath string) (int, error) {
	socketRef, err := socketRefForPath(socketPath)
	if err == nil {
		if pid, err := pidBySocketRef(socketRef); err == nil {
			return pid, nil
		}
	}

	if pid, err := pidByCmdline(socketPath); err == nil {
		return pid, nil
	}

	if err != nil {
		return 0, err
	}

	return 0, fmt.Errorf("resolve process pid for socket %s: no owning process found", socketPath)
}

func pidBySocketRef(socketRef string) (int, error) {
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}

	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		fdEntries, err := os.ReadDir(filepath.Join("/proc", entry.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fdEntry := range fdEntries {
			target, err := os.Readlink(filepath.Join("/proc", entry.Name(), "fd", fdEntry.Name()))
			if err != nil {
				continue
			}
			if strings.TrimSpace(target) == socketRef {
				return pid, nil
			}
		}
	}

	return 0, fmt.Errorf("resolve process pid for %s: no owning process found", socketRef)
}

func pidByCmdline(socketPath string) (int, error) {
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}

	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		if strings.Contains(string(cmdline), socketPath) {
			return pid, nil
		}
	}

	return 0, fmt.Errorf("resolve process pid for socket %s: no matching command line found", socketPath)
}

func socketRefForPath(socketPath string) (string, error) {
	file, err := os.Open("/proc/net/unix")
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
