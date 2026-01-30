package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{100, "100 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
		{1649267441664, "1.5 TB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatBytes(tt.bytes)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatMB(t *testing.T) {
	tests := []struct {
		mb       int64
		expected string
	}{
		{512, "512 MB"},
		{1024, "1.0 GB"},
		{2048, "2.0 GB"},
		{6144, "6.0 GB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatMB(tt.mb)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatBps(t *testing.T) {
	// The API returns network_download_bps in BYTES per second
	// The CLI should convert to bits and display as Mbps/Gbps
	// Formula: bytes/sec * 8 = bits/sec
	tests := []struct {
		name         string
		bytesPerSec  int64
		expected     string
	}{
		// 30 Mbps = 30,000,000 bits/sec = 3,750,000 bytes/sec
		// This is the user's reported bug: they set 30Mbps, API stores 3750000 bytes/sec,
		// CLI was incorrectly showing "4 Mbps" instead of "30 Mbps"
		{"30Mbps bandwidth limit", 3750000, "30 Mbps"},

		// 1 Gbps = 1,000,000,000 bits/sec = 125,000,000 bytes/sec
		{"1Gbps bandwidth limit", 125000000, "1.0 Gbps"},

		// 100 Mbps = 100,000,000 bits/sec = 12,500,000 bytes/sec
		{"100Mbps bandwidth limit", 12500000, "100 Mbps"},

		// 500 Mbps = 500,000,000 bits/sec = 62,500,000 bytes/sec
		{"500Mbps bandwidth limit", 62500000, "500 Mbps"},

		// 10 Gbps = 10,000,000,000 bits/sec = 1,250,000,000 bytes/sec
		{"10Gbps bandwidth limit", 1250000000, "10.0 Gbps"},

		// Small values: 1 Mbps = 1,000,000 bits/sec = 125,000 bytes/sec
		{"1Mbps bandwidth limit", 125000, "1 Mbps"},

		// Very small: 100 Kbps = 100,000 bits/sec = 12,500 bytes/sec
		{"100Kbps bandwidth limit", 12500, "100 Kbps"},

		// Tiny: 8000 bits/sec = 1000 bytes/sec
		{"8Kbps bandwidth limit", 1000, "8 Kbps"},

		// Edge case: 0 bytes/sec
		{"zero bandwidth", 0, "0 bps"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatBps(tt.bytesPerSec)
			assert.Equal(t, tt.expected, result)
		})
	}
}
