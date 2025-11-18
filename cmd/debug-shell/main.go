package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/onkernel/hypeman/lib/system"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run cmd/debug-shell/main.go <socket-path> [command...]")
		fmt.Println("Example: go run cmd/debug-shell/main.go /tmp/.../vsock.sock")
		fmt.Println("Example: go run cmd/debug-shell/main.go /tmp/.../vsock.sock ls -la /")
		os.Exit(1)
	}
	socketPath := os.Args[1]
	
	command := []string{"/bin/sh"}
	if len(os.Args) > 2 {
		command = os.Args[2:]
	}

	// Handle signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	fmt.Printf("Connecting to %s...\n", socketPath)
	
	// Determine if we should use TTY
	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	if len(os.Args) > 2 {
		// If running a command, don't use TTY unless explicitly interactive?
		// Usually running a command (like ls) is non-interactive TTY wise unless forced.
		// Let's default to TTY=false if arguments provided, to simplify probing.
		isTTY = false
	}

	var oldState *term.State
	var err error
	if isTTY {
		// Put terminal in raw mode for interactive shell
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			fmt.Printf("Warning: could not make terminal raw: %v\n", err)
			isTTY = false
		} else {
			defer term.Restore(int(os.Stdin.Fd()), oldState)
		}
	}

	// Start shell
	status, err := system.ExecIntoInstance(ctx, socketPath, system.ExecOptions{
		Command: command,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		TTY:     isTTY,
	})

	if err != nil {
		// Restore terminal before printing error
		if oldState != nil {
			term.Restore(int(os.Stdin.Fd()), oldState)
		}
		fmt.Printf("\r\nError: %v\n", err)
		os.Exit(1)
	}
	
	// Restore terminal
	if oldState != nil {
		term.Restore(int(os.Stdin.Fd()), oldState)
	}
	fmt.Printf("\r\nExit code: %d\n", status.Code)
}

