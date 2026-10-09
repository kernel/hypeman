//go:build darwin

package main

import (
	"context"
	"testing"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDarwinNetworkReconfigurationUnsupported(t *testing.T) {
	response, err := (&guestServer{}).ReconfigureNetwork(context.Background(), &pb.ReconfigureNetworkRequest{})
	require.Nil(t, response)
	require.Equal(t, codes.Unimplemented, status.Code(err))
}
