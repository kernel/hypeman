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
// socket for the given hypervisor control path, via the socket inode in
// /proc/net/unix and each process's fd table. The fd scan requires the
// caller to hold CAP_SYS_PTRACE (or run as root) so no live owner is missed;
// an ErrNoOwningProcess result is proof the listener is gone.
func ResolveProcessPID(socketPath string) (pid int, err error) {
	return resolveProcessPID(socketPath, 0)
}

// ResolveProcessPIDForOwner resolves a socket while preferring an expected
// owner when the socket descriptor is temporarily shared with a child process.
func ResolveProcessPIDForOwner(socketPath string, ownerPID int) (pid int, err error) {
	return resolveProcessPID(socketPath, ownerPID)
}

func resolveProcessPID(socketPath string, ownerPID int) (pid int, err error) {
	socketRef, err := socketRefForPath(socketPath)
	if err != nil {
		return 0, err
	}
	// Confirm the expected owner first so a live stored PID does not
	// require scanning every process fd.
	if ownerPID > 0 && processHoldsSocketRef(ownerPID, socketRef) {
		return ownerPID, nil
	}
	return pidBySocketRef(socketRef, ownerPID)
}

func processHoldsSocketRef(pid int, socketRef string) bool {
	fdEntries, err := os.ReadDir(filepath.Join(procDir, strconv.Itoa(pid), "fd"))
	if err != nil {
		return false
	}
	for _, fdEntry := range fdEntries {
		target, err := os.Readlink(filepath.Join(procDir, strconv.Itoa(pid), "fd", fdEntry.Name()))
		if err != nil {
			// Skip fds that cannot be read, like the full scan does: an fd
			// vanishing mid-scan must not hide a listener held by a later fd.
			continue
		}
		if strings.TrimSpace(target) == socketRef {
			return true
		}
	}
	return false
}

func pidBySocketRef(socketRef string, ownerPID int) (int, error) {
	procEntries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}

	var owners []int
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
				owners = append(owners, pid)
				break
			}
		}
	}

	if ownerPID > 0 {
		for _, pid := range owners {
			if pid == ownerPID {
				return pid, nil
			}
		}
	}
	if len(owners) == 1 {
		return owners[0], nil
	}
	if len(owners) > 1 {
		return 0, fmt.Errorf("resolve process pid for %s: multiple owning processes found: %v", socketRef, owners)
	}
	if scanErr != nil {
		return 0, fmt.Errorf("resolve process pid for %s: inspect process fds: %w", socketRef, scanErr)
	}
	return 0, fmt.Errorf("resolve process pid for %s: %w", socketRef, ErrNoOwningProcess)
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
