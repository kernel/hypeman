package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/vmconfig"
	"github.com/stretchr/testify/assert"
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

func TestInstallEgressProxyCA_SkipConditions(t *testing.T) {
	t.Parallel()
	log := &Logger{console: os.Stdout}

	t.Run("nil config", func(t *testing.T) {
		require.NoError(t, installEgressProxyCA(log, nil))
	})

	t.Run("nil egress proxy", func(t *testing.T) {
		cfg := &vmconfig.Config{}
		require.NoError(t, installEgressProxyCA(log, cfg))
	})

	t.Run("egress proxy disabled", func(t *testing.T) {
		cfg := &vmconfig.Config{
			EgressProxy: &vmconfig.EgressProxyConfig{Enabled: false, CACertPEM: "pem-data"},
		}
		require.NoError(t, installEgressProxyCA(log, cfg))
	})

	t.Run("empty CA cert PEM", func(t *testing.T) {
		cfg := &vmconfig.Config{
			EgressProxy: &vmconfig.EgressProxyConfig{Enabled: true, CACertPEM: ""},
		}
		require.NoError(t, installEgressProxyCA(log, cfg))
	})
}

func TestInstallEgressProxyCA_UpdateCACertificatesFailure(t *testing.T) {
	// Use a temp dir as the PATH so we control which update-ca-certificates binary is found.
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)

	// Create an update-ca-certificates that exits non-zero.
	toolPath := filepath.Join(binDir, "update-ca-certificates")
	require.NoError(t, os.WriteFile(toolPath, []byte("#!/bin/sh\nexit 1\n"), 0755))

	// We need to write the CA cert to the real path, which requires root.
	// Instead, test that the function returns an error when update-ca-certificates fails.
	// Skip if we can't write to the CA directory (non-root in test environment).
	caDir := "/usr/local/share/ca-certificates"
	if err := os.MkdirAll(caDir, 0755); err != nil {
		t.Skipf("cannot create %s (need root): %v", caDir, err)
	}
	caPath := filepath.Join(caDir, "hypeman-egress-proxy.crt")
	// Clean up after test
	t.Cleanup(func() { os.Remove(caPath) })

	log := &Logger{console: os.Stdout}
	cfg := &vmconfig.Config{
		EgressProxy: &vmconfig.EgressProxyConfig{
			Enabled:   true,
			CACertPEM: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
		},
	}

	err := installEgressProxyCA(log, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update-ca-certificates")
}

func TestInstallEgressProxyCA_MissingUpdateCACertificatesIsNotFatal(t *testing.T) {
	// When update-ca-certificates is not found, installEgressProxyCA should
	// succeed (return nil) — the cert file is still written, just the trust
	// store refresh is skipped.
	binDir := t.TempDir()
	t.Setenv("PATH", binDir) // Empty PATH — no update-ca-certificates

	caDir := "/usr/local/share/ca-certificates"
	if err := os.MkdirAll(caDir, 0755); err != nil {
		t.Skipf("cannot create %s (need root): %v", caDir, err)
	}
	caPath := filepath.Join(caDir, "hypeman-egress-proxy.crt")
	t.Cleanup(func() { os.Remove(caPath) })

	log := &Logger{console: os.Stdout}
	cfg := &vmconfig.Config{
		EgressProxy: &vmconfig.EgressProxyConfig{
			Enabled:   true,
			CACertPEM: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
		},
	}

	err := installEgressProxyCA(log, cfg)
	require.NoError(t, err, "missing update-ca-certificates should not be fatal")

	// Verify the cert file was written
	data, readErr := os.ReadFile(caPath)
	require.NoError(t, readErr)
	assert.Equal(t, cfg.EgressProxy.CACertPEM, string(data))
}
