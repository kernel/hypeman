//go:build linux

package hypervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	match, err := socketMatcherForPath(socketPath)
	if err != nil {
		return 0, err
	}
	// Confirm the expected owner first so a live stored PID does not
	// require scanning every process fd.
	if ownerPID > 0 && processHoldsSocket(ownerPID, match) {
		return ownerPID, nil
	}
	return pidBySocket(socketPath, match, ownerPID)
}

// socketMatcher reports whether fd target ("socket:[inode]") held by pid is
// the listener being resolved.
type socketMatcher func(pid int, target string) (bool, error)

// socketMatcherForPath matches the listener bound to socketPath. A VMM that
// runs in its own root, such as a jailed firecracker, binds its socket inside
// that root, so /proc/net/unix lists the in-root path and socketPath is a link
// to it. When no listener is listed under socketPath, a listener matches if
// its path, resolved in the holding process's root, is the file socketPath
// names.
func socketMatcherForPath(socketPath string) (socketMatcher, error) {
	socketRef, err := socketRefForPath(socketPath)
	if err == nil {
		return func(_ int, target string) (bool, error) { return target == socketRef, nil }, nil
	}
	if !errors.Is(err, ErrNoOwningProcess) {
		return nil, err
	}
	want, statErr := os.Stat(socketPath)
	if statErr != nil || want.Mode()&os.ModeSocket == 0 {
		return nil, err
	}
	listeners, listErr := listeningSocketPaths()
	if listErr != nil {
		return nil, listErr
	}
	return func(pid int, target string) (bool, error) {
		path, ok := listeners[target]
		if !ok {
			return false, nil
		}
		got, err := os.Stat(filepath.Join(procDir, strconv.Itoa(pid), "root", path))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				return false, nil
			}
			return false, err
		}
		return os.SameFile(want, got), nil
	}, nil
}

func processHoldsSocket(pid int, match socketMatcher) bool {
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
		if ok, _ := match(pid, strings.TrimSpace(target)); ok {
			return true
		}
	}
	return false
}

func pidBySocket(socketPath string, match socketMatcher, ownerPID int) (int, error) {
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
			ok, err := match(pid, strings.TrimSpace(target))
			if err != nil {
				scanErr = err
				continue
			}
			if ok {
				owners = append(owners, pid)
				break
			}
		}
	}

	// The scan observed ownerPID holding the listener fd — the same evidence
	// the fast path uses — so a child transiently sharing the inherited fd
	// must not turn a proven owner into an error.
	if ownerPID > 0 && slices.Contains(owners, ownerPID) {
		return ownerPID, nil
	}
	if len(owners) == 1 {
		return owners[0], nil
	}
	if len(owners) > 1 {
		return 0, fmt.Errorf("resolve process pid for %s: multiple owning processes found: %v", socketPath, owners)
	}
	if scanErr != nil {
		return 0, fmt.Errorf("resolve process pid for %s: inspect process fds: %w", socketPath, scanErr)
	}
	return 0, fmt.Errorf("resolve process pid for %s: %w", socketPath, ErrNoOwningProcess)
}

// listeningSocketPaths maps each listening socket with a filesystem path to that path.
func listeningSocketPaths() (map[string]string, error) {
	paths := make(map[string]string)
	err := scanListeners(func(ref, path string) error {
		if strings.HasPrefix(path, "/") {
			paths[ref] = path
		}
		return nil
	})
	return paths, err
}

func socketRefForPath(socketPath string) (string, error) {
	var socketRef string
	err := scanListeners(func(ref, path string) error {
		if path != socketPath {
			return nil
		}
		if socketRef != "" {
			return fmt.Errorf("resolve process pid for socket %s: multiple socket inodes found", socketPath)
		}
		socketRef = ref
		return nil
	})
	if err != nil {
		return "", err
	}
	if socketRef != "" {
		return socketRef, nil
	}
	return "", fmt.Errorf("resolve process pid for socket %s: socket inode not found: %w", socketPath, ErrNoOwningProcess)
}

// scanListeners calls fn with the fd target ("socket:[inode]") and bound path
// of each listening socket in /proc/net/unix.
func scanListeners(fn func(ref, path string) error) error {
	file, err := os.Open(filepath.Join(procDir, "net", "unix"))
	if err != nil {
		return fmt.Errorf("open /proc/net/unix: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[0] == "Num" {
			continue
		}
		// Accepted server-side sockets list the bound path too; only the
		// listener identifies the owning process.
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&soAcceptcon == 0 {
			continue
		}
		if err := fn(fmt.Sprintf("socket:[%s]", fields[6]), fields[len(fields)-1]); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan /proc/net/unix: %w", err)
	}
	return nil
}
