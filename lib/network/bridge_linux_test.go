//go:build linux

package network

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseBridgeFilters(t *testing.T) {
	output := `filter parent 1: protocol all pref 1 basic chain 0
filter parent 1: protocol all pref 1 basic chain 0 handle 0x1 flowid 1:a3f2
  meta(rt_iif eq 42)
filter parent 1: protocol all pref 1 basic chain 0 handle 0x2 flowid 1:b001
  meta(rt_iif eq 57)
filter parent 1: protocol all pref 1 basic chain 0 handle 0x3 flowid 1:000c
`

	filters := parseBridgeFilters(output)
	assert.Equal(t, []bridgeFilter{
		{handle: "", flowID: "", rtIif: -1}, // chain header line, no handle
		{handle: "0x1", flowID: "1:a3f2", rtIif: 42},
		{handle: "0x2", flowID: "1:b001", rtIif: 57},
		{handle: "0x3", flowID: "1:000c", rtIif: -1}, // ematch line missing
	}, filters)
}

func TestParseBridgeFiltersEmpty(t *testing.T) {
	assert.Empty(t, parseBridgeFilters(""))
}
