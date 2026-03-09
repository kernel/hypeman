package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalReadyFD(t *testing.T) {
	readyReader, readyWriter, err := os.Pipe()
	require.NoError(t, err)
	defer readyReader.Close()
	defer readyWriter.Close()

	t.Setenv(readyFDEnv, fmt.Sprintf("%d", readyWriter.Fd()))
	err = signalReadyFD()
	require.NoError(t, err)

	buf := make([]byte, 1)
	n, err := readyReader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, byte(1), buf[0])
}

func TestSignalReadyFDInvalid(t *testing.T) {
	t.Setenv(readyFDEnv, "not-an-int")
	err := signalReadyFD()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}
