package main

import (
	"context"
	"errors"
	"syscall"
	"testing"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDarwinShutdownPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		signal       int32
		euid         int
		canceled     bool
		commandError error
		code         codes.Code
		called       bool
	}{
		{name: "default orderly shutdown", called: true},
		{name: "explicit orderly shutdown", signal: int32(syscall.SIGTERM), called: true},
		{name: "reject arbitrary signal", signal: int32(syscall.SIGKILL), code: codes.InvalidArgument},
		{name: "reject desktop agent", euid: 501, code: codes.PermissionDenied},
		{name: "canceled before command", canceled: true, code: codes.Canceled},
		{name: "command failure", commandError: errors.New("shutdown refused"), code: codes.Internal, called: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			called := false
			response, err := requestDarwinShutdown(ctx, &pb.ShutdownRequest{Signal: tc.signal}, tc.euid, func(context.Context) error {
				called = true
				return tc.commandError
			})
			require.Equal(t, tc.called, called)
			require.Equal(t, tc.code, status.Code(err))
			if tc.code == codes.OK {
				require.NotNil(t, response)
			} else {
				require.Nil(t, response)
			}
		})
	}
}
