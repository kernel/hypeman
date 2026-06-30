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

	assert.Equal(t, []bridgeFilter{
		{handle: "0x1", flowID: "1:a3f2", rtIif: 42},
		{handle: "0x2", flowID: "1:b001", rtIif: 57},
		{handle: "0x3", flowID: "1:000c", rtIif: -1},
	}, parseBridgeFilters(output))
}

func TestParseBridgeFiltersEmpty(t *testing.T) {
	assert.Empty(t, parseBridgeFilters(""))
}

func TestPlanOrphanedBridgeTC(t *testing.T) {
	staleFilters, staleClasses, safe := planOrphanedBridgeTC(
		map[int]bool{42: true},
		[]bridgeFilter{
			{handle: "0x1", flowID: "1:a3f2", rtIif: 42},
			{handle: "0x2", flowID: "1:b001", rtIif: 57},
			{handle: "0x3", flowID: "1:000c", rtIif: -1},
		},
		[]string{"1:1", "1:a3f2", "1:b001", "1:000c", "1:9999"},
	)

	assert.True(t, safe)
	assert.Equal(t, []bridgeFilter{
		{handle: "0x2", flowID: "1:b001", rtIif: 57},
		{handle: "0x3", flowID: "1:000c", rtIif: -1},
	}, staleFilters)
	assert.Equal(t, []string{"1:b001", "1:000c", "1:9999"}, staleClasses)
}

func TestPlanOrphanedBridgeTCBailsWhenNoRTIIFParses(t *testing.T) {
	staleFilters, staleClasses, safe := planOrphanedBridgeTC(
		map[int]bool{42: true},
		[]bridgeFilter{{handle: "0x1", flowID: "1:a3f2", rtIif: -1}},
		[]string{"1:a3f2"},
	)

	assert.False(t, safe)
	assert.Nil(t, staleFilters)
	assert.Nil(t, staleClasses)
}
