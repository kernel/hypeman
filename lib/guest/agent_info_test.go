package guest

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// bufconnDialer reaches an in-process agent.
type bufconnDialer struct{ listener *bufconn.Listener }

func (d bufconnDialer) Key() string { return "agent-info-test" }
func (d bufconnDialer) DialVsock(ctx context.Context, _ int) (net.Conn, error) {
	return d.listener.DialContext(ctx)
}

type infoAgent struct {
	UnimplementedGuestServiceServer
	info *GetAgentInfoResponse
}

func (a infoAgent) GetAgentInfo(context.Context, *GetAgentInfoRequest) (*GetAgentInfoResponse, error) {
	return a.info, nil
}

func serveAgent(t *testing.T, agent GuestServiceServer) hypervisor.VsockDialer {
	t.Helper()
	listener := bufconn.Listen(1 << 16)
	server := grpc.NewServer()
	RegisterGuestServiceServer(server, agent)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return bufconnDialer{listener: listener}
}

func TestGetAgentInfo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := GetAgentInfo(ctx, serveAgent(t, infoAgent{info: &GetAgentInfoResponse{ProtocolVersion: 1, Version: "v1.2.3"}}))
	require.NoError(t, err)
	require.EqualValues(t, 1, info.ProtocolVersion)
	require.Equal(t, "v1.2.3", info.Version)

	info, err = GetAgentInfo(ctx, serveAgent(t, &UnimplementedGuestServiceServer{}))
	require.NoError(t, err, "an agent that predates the RPC is not an error")
	require.Zero(t, info.ProtocolVersion, "it reports protocol 0")
	require.Empty(t, info.Capabilities)
}
