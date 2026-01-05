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

func TestFormatTcRate(t *testing.T) {
	tests := []struct {
		name        string
		bytesPerSec int64
		want        string
	}{
		// Exact gbit values
		{
			name:        "1 Gbps exactly",
			bytesPerSec: 125000000, // 1 Gbps = 1,000,000,000 bits/s = 125,000,000 bytes/s
			want:        "1gbit",
		},
		{
			name:        "10 Gbps exactly",
			bytesPerSec: 1250000000,
			want:        "10gbit",
		},
		// Non-round gbit values should use mbit
		{
			name:        "2.5 Gbps uses mbit to avoid truncation",
			bytesPerSec: 312500000, // 2.5 Gbps = 2,500,000,000 bits/s
			want:        "2500mbit",
		},
		{
			name:        "1.5 Gbps uses mbit",
			bytesPerSec: 187500000, // 1.5 Gbps = 1,500,000,000 bits/s
			want:        "1500mbit",
		},
		// Exact mbit values
		{
			name:        "100 Mbps exactly",
			bytesPerSec: 12500000, // 100 Mbps = 100,000,000 bits/s
			want:        "100mbit",
		},
		{
			name:        "500 Mbps exactly",
			bytesPerSec: 62500000,
			want:        "500mbit",
		},
		// Non-round mbit values should use kbit
		{
			name:        "1.5 Mbps uses kbit",
			bytesPerSec: 187500, // 1.5 Mbps = 1,500,000 bits/s
			want:        "1500kbit",
		},
		// Exact kbit values
		{
			name:        "100 Kbps exactly",
			bytesPerSec: 12500, // 100 Kbps = 100,000 bits/s
			want:        "100kbit",
		},
		// Non-round kbit values should use bit
		{
			name:        "1.5 Kbps uses bit",
			bytesPerSec: 187, // 1,496 bits/s (not evenly divisible by 1000)
			want:        "1496bit",
		},
		// Small values
		{
			name:        "small value in bits",
			bytesPerSec: 100,
			want:        "800bit",
		},
		// Zero
		{
			name:        "zero bytes",
			bytesPerSec: 0,
			want:        "0bit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTcRate(tt.bytesPerSec)
			assert.Equal(t, tt.want, got)
		})
	}
}

