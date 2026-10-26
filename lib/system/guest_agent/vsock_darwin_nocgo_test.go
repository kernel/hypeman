//go:build darwin && !cgo

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinVsockRequiresCGO(t *testing.T) {
	listener, err := listenVsock(2222)
	require.Nil(t, listener)
	require.ErrorContains(t, err, "cgo-enabled build")
}
