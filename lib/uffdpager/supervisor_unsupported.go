//go:build !linux

package uffdpager

import (
	"context"
	"fmt"
)

type Supervisor struct{}

func NewSupervisor(context.Context, string, int64) (*Supervisor, error) {
	return nil, fmt.Errorf("uffd pager is only supported on linux")
}

func (s *Supervisor) VersionKey() string {
	return ""
}

func (s *Supervisor) CreateSession(context.Context, CreateSessionRequest) (*CreateSessionResponse, error) {
	return nil, fmt.Errorf("uffd pager is only supported on linux")
}

func (s *Supervisor) CloseSession(context.Context, string) error {
	return nil
}

func (s *Supervisor) CloseSessionVersion(context.Context, string, string) error {
	return nil
}

func (s *Supervisor) Stats(context.Context) (*Stats, error) {
	return nil, fmt.Errorf("uffd pager is only supported on linux")
}

func (s *Supervisor) HealthVersion(context.Context, string) (*HealthResponse, error) {
	return nil, fmt.Errorf("uffd pager is only supported on linux")
}

func Main([]string) error {
	return fmt.Errorf("uffd pager is only supported on linux")
}
