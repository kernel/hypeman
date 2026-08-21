//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "HypemanGuestAgent"

func runPlatform(run func() error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service: %w", err)
	}
	if !isService {
		return run()
	}
	return svc.Run(windowsServiceName, &guestAgentService{run: run})
}

type guestAgentService struct {
	run func() error
}

func (s *guestAgentService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}
	errCh := make(chan error, 1)
	go func() { errCh <- s.run() }()
	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-errCh:
			if err != nil {
				return true, 1
			}
			return false, 0
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				return false, 0
			}
		}
	}
}
