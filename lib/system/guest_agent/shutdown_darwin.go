//go:build darwin

package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"syscall"

	pb "github.com/kernel/hypeman/lib/guest"
)

// Darwin's launchd is not Linux init: sending it SIGTERM is not a shutdown API.
func (s *guestServer) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	return requestDarwinShutdown(ctx, req, os.Geteuid(), func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		command := darwinShutdownCommand()
		if err := command.Start(); err != nil {
			return err
		}
		go func() {
			if err := command.Wait(); err != nil {
				log.Printf("scheduled Darwin shutdown failed: %v", err)
			}
		}()
		return nil
	})
}

func darwinShutdownCommand() *exec.Cmd {
	// A short detached delay lets gRPC send the acceptance reply before macOS
	// tears down vsock. The host must still independently confirm owned VMM exit.
	// Once accepted, shutdown is not cancelled by the RPC connection disappearing.
	command := exec.Command("/bin/sh", "-c", "/bin/sleep 1; exec /sbin/shutdown -h now")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return command
}
