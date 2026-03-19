package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLookupUpdateCACertificatesPath(t *testing.T) {
	t.Run("returns not found when tool is absent from PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())

		_, err := lookupUpdateCACertificatesPath()
		require.Error(t, err)
	})

	t.Run("finds update-ca-certificates from PATH", func(t *testing.T) {
		binDir := t.TempDir()
		t.Setenv("PATH", binDir)

		toolPath := filepath.Join(binDir, "update-ca-certificates")
		require.NoError(t, os.WriteFile(toolPath, []byte("#!/bin/sh\nexit 0\n"), 0755))

		foundPath, err := lookupUpdateCACertificatesPath()
		require.NoError(t, err)
		require.Equal(t, toolPath, foundPath)
	})
}
