//go:build !windows

package cmd

import (
	"os"

	"golang.org/x/sys/unix"
)

// In order to avoid large buffers on pipes, this function create a pair of
// files for reading and writing through a barely buffered socket.
func createSocketPair() (*os.File, *os.File, bool, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, false, err
	}

	parentSock, childSock := fds[0], fds[1]

	// Use small buffer sizes so we don't ask the server for more paginated
	// values than we actually need.
	if err := unix.SetsockoptInt(parentSock, unix.SOL_SOCKET, unix.SO_SNDBUF, 128); err != nil {
		return nil, nil, false, err
	}
	if err := unix.SetsockoptInt(childSock, unix.SOL_SOCKET, unix.SO_RCVBUF, 128); err != nil {
		return nil, nil, false, err
	}

	pagerInput := os.NewFile(uintptr(childSock), "child_socket")
	outputFile := os.NewFile(uintptr(parentSock), "parent_socket")
	return pagerInput, outputFile, true, nil
}
