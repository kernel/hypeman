package network

import (
	"context"
	"crypto/rand"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
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
			want:       "tap-abcd1234",
		},
		{
			name:       "longer ID truncates",
			instanceID: "abcd1234efgh5678",
			want:       "tap-abcd1234",
		},
		{
			name:       "uppercase converted to lowercase",
			instanceID: "ABCD1234",
			want:       "tap-abcd1234",
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

func TestSubnetsOverlap(t *testing.T) {
	tests := []struct {
		name    string
		subnet1 string
		subnet2 string
		overlap bool
	}{
		{
			name:    "same subnet",
			subnet1: "192.168.1.0/24",
			subnet2: "192.168.1.0/24",
			overlap: true,
		},
		{
			name:    "non-overlapping /24",
			subnet1: "192.168.1.0/24",
			subnet2: "192.168.2.0/24",
			overlap: false,
		},
		{
			name:    "larger subnet contains smaller",
			subnet1: "192.168.0.0/16",
			subnet2: "192.168.1.0/24",
			overlap: true,
		},
		{
			name:    "smaller subnet within larger",
			subnet1: "192.168.1.0/24",
			subnet2: "192.168.0.0/16",
			overlap: true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := subnetsOverlap(tt.subnet1, tt.subnet2)
			assert.Equal(t, tt.overlap, got)
		})
	}
}

func TestValidateNetworkName(t *testing.T) {
	tests := []struct {
		name    string
		netName string
		wantErr bool
	}{
		{
			name:    "valid lowercase",
			netName: "default",
			wantErr: false,
		},
		{
			name:    "valid with dash",
			netName: "my-network",
			wantErr: false,
		},
		{
			name:    "valid with numbers",
			netName: "net-123",
			wantErr: false,
		},
		{
			name:    "invalid - starts with dash",
			netName: "-network",
			wantErr: true,
		},
		{
			name:    "invalid - ends with dash",
			netName: "network-",
			wantErr: true,
		},
		{
			name:    "invalid - uppercase",
			netName: "Network",
			wantErr: true,
		},
		{
			name:    "invalid - empty",
			netName: "",
			wantErr: true,
		},
		{
			name:    "invalid - too long",
			netName: "this-network-name-is-way-too-long-and-exceeds-the-sixty-three-character-limit-for-names",
			wantErr: true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNetworkName(tt.netName)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGetFirstIP(t *testing.T) {
	tests := []struct {
		name   string
		subnet string
		want   string
	}{
		{
			name:   "class C network",
			subnet: "192.168.100.0/24",
			want:   "192.168.100.1",
		},
		{
			name:   "class B network",
			subnet: "172.16.0.0/16",
			want:   "172.16.0.1",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getFirstIP(tt.subnet)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Helper to parse IP
func parseIP(s string) net.IP {
	return net.ParseIP(s).To4()
}

