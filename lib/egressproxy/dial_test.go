package egressproxy

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

type fixedResolver map[string][]netip.Addr

func (r fixedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r[host], nil
}

func TestIsPublicEgressIP(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true,
		"10.0.0.1":             false,
		"100.64.0.1":           false,
		"127.0.0.1":            false,
		"169.254.169.254":      false,
		"192.0.2.1":            false,
		"198.18.0.1":           false,
		"224.0.0.1":            false,
		"::1":                  false,
		"fc00::1":              false,
		"fe80::1":              false,
		"2001:db8::1":          false,
	}
	for address, want := range tests {
		t.Run(address, func(t *testing.T) {
			require.Equal(t, want, isPublicEgressIP(netip.MustParseAddr(address)))
		})
	}
}

func TestPublicDialContext(t *testing.T) {
	t.Parallel()

	resolver := fixedResolver{
		"public.example":  {netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("8.8.8.8")},
		"private.example": {netip.MustParseAddr("10.0.0.1")},
		"mixed.example":   {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")},
	}
	var dialed string
	dial := publicDialContext(resolver, func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		client, server := net.Pipe()
		server.Close()
		return client, nil
	})

	connection, err := dial(context.Background(), "tcp", "public.example:443")
	require.NoError(t, err)
	require.Equal(t, "8.8.8.8:443", dialed)
	require.NoError(t, connection.Close())

	for _, address := range []string{"private.example:443", "mixed.example:443", "127.0.0.1:443"} {
		dialed = ""
		connection, err := dial(context.Background(), "tcp", address)
		require.Error(t, err)
		require.Nil(t, connection)
		require.Empty(t, dialed)
	}
}
