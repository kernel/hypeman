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
	tests := []struct {
		bps      int64
		expected string
	}{
		{500, "500 bps"},
		{1000, "1 Kbps"},
		{1000000, "1 Mbps"},
		{1000000000, "1.0 Gbps"},
		{125000000, "125 Mbps"},
		{10000000000, "10.0 Gbps"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatBps(tt.bps)
			assert.Equal(t, tt.expected, result)
		})
	}
}
