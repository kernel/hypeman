//go:build linux

package hypervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

var procDir = "/proc"

// soAcceptcon marks a listening socket in /proc/net/unix (__SO_ACCEPTCON).
const soAcceptcon = 0x10000

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
	return 0, false, fmt.Errorf("resolve process pid for socket %s: %w", socketPath, ErrNoOwningProcess)
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
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			scanErr = err
			continue
		}
		for _, fdEntry := range fdEntries {
			target, err := os.Readlink(filepath.Join(procDir, entry.Name(), "fd", fdEntry.Name()))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
					continue
				}
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
	return 0, fmt.Errorf("resolve process pid for %s: %w", socketRef, ErrNoOwningProcess)
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
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
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
	return 0, fmt.Errorf("resolve process pid for socket %s: no matching command line found: %w", socketPath, ErrNoOwningProcess)
}

func socketRefForPath(socketPath string) (string, error) {
	file, err := os.Open(filepath.Join(procDir, "net", "unix"))
	if err != nil {
		return "", fmt.Errorf("open /proc/net/unix: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var socketRef string
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
		// Accepted server-side sockets list the bound path too; only the
		// listener identifies the owning process.
		flags, parseErr := strconv.ParseUint(fields[3], 16, 32)
		if parseErr != nil || flags&soAcceptcon == 0 {
			continue
		}
		inode := fields[6]
		if inode == "" {
			break
		}
		if socketRef != "" {
			return "", fmt.Errorf("resolve process pid for socket %s: multiple socket inodes found", socketPath)
		}
		socketRef = fmt.Sprintf("socket:[%s]", inode)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan /proc/net/unix: %w", err)
	}
	if socketRef != "" {
		return socketRef, nil
	}
	return "", fmt.Errorf("resolve process pid for socket %s: socket inode not found: %w", socketPath, ErrNoOwningProcess)
}
