//go:build linux

package network

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBridgeFiltersForTAP(t *testing.T) {
	output := `filter parent 1: protocol all pref 1 basic chain 0
filter parent 1: protocol all pref 1 basic chain 0 handle 0x1 flowid 1:a3f2
  meta(rt_iif eq 42)
filter parent 1: protocol all pref 1 basic chain 0 handle 0x2 flowid 1:b001
  meta(rt_iif eq 57)
filter parent 1: protocol all pref 1 basic chain 0 handle 0x3 flowid 1:000c
`

	assert.Equal(t, []bridgeFilter{
		{handle: "0x1", flowID: "1:a3f2", rtIif: 42},
	}, bridgeFiltersForTAP(output, 42))
}

func TestBridgeFiltersForTAPEmpty(t *testing.T) {
	assert.Empty(t, bridgeFiltersForTAP("", 42))
}
