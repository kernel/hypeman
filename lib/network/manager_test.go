package network

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateMAC(t *testing.T) {
	// Generate 100 MACs to test uniqueness and format
	seen := make(map[string]bool)
	
	for i := 0; i < 100; i++ {
		mac, err := generateMAC()
		require.NoError(t, err)
		
		// Check format (XX:XX:XX:XX:XX:XX)
		require.Len(t, mac, 17, "MAC should be 17 chars")
		
		// Check starts with 02:00:00 (locally administered)
		require.True(t, mac[:8] == "02:00:00", "MAC should start with 02:00:00")
		
		// Check uniqueness
		require.False(t, seen[mac], "MAC should be unique")
		seen[mac] = true
	}
}

func TestGenerateTAPName(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		want       string
	}{
		{
			name:       "8 char ID",
			instanceID: "abcd1234",
			want:       "hype-abcd1234",
		},
		{
			name:       "longer ID truncates",
			instanceID: "abcd1234efgh5678",
			want:       "hype-abcd1234",
		},
		{
			name:       "uppercase converted to lowercase",
			instanceID: "ABCD1234",
			want:       "hype-abcd1234",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateTAPName(tt.instanceID)
			assert.Equal(t, tt.want, got)
			// Verify within Linux interface name limit (15 chars)
			assert.LessOrEqual(t, len(got), 15)
		})
	}
}

func TestIncrementIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		n    int
		want string
	}{
		{
			name: "increment by 1",
			ip:   "192.168.1.10",
			n:    1,
			want: "192.168.1.11",
		},
		{
			name: "increment by 10",
			ip:   "192.168.1.10",
			n:    10,
			want: "192.168.1.20",
		},
		{
			name: "overflow to next subnet",
			ip:   "192.168.1.255",
			n:    1,
			want: "192.168.2.0",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := parseIP(tt.ip)
			got := incrementIP(ip, tt.n)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func TestDeriveGateway(t *testing.T) {
	tests := []struct {
		name    string
		cidr    string
		want    string
		wantErr bool
	}{
		{
			name: "/16 subnet",
			cidr: "10.100.0.0/16",
			want: "10.100.0.1",
		},
		{
			name: "/24 subnet",
			cidr: "192.168.1.0/24",
			want: "192.168.1.1",
		},
		{
			name: "/8 subnet",
			cidr: "10.0.0.0/8",
			want: "10.0.0.1",
		},
		{
			name: "different starting point",
			cidr: "172.30.0.0/16",
			want: "172.30.0.1",
		},
		{
			name:    "invalid CIDR",
			cidr:    "not-a-cidr",
			wantErr: true,
		},
		{
			name:    "missing prefix",
			cidr:    "10.100.0.0",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveGateway(tt.cidr)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Helper to parse IP
func parseIP(s string) net.IP {
	return net.ParseIP(s).To4()
}

