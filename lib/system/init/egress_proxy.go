package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kernel/hypeman/lib/vmconfig"
)

// installEgressProxyCA installs the egress proxy CA certificate into the guest
// trust store. Returns nil if egress proxy is not configured or if the CA cert
// was installed successfully. Returns an error if any step fails.
func installEgressProxyCA(log *Logger, cfg *vmconfig.Config) error {
	if cfg == nil || cfg.EgressProxy == nil || !cfg.EgressProxy.Enabled || cfg.EgressProxy.CACertPEM == "" {
		return nil
	}

	caPath := "/usr/local/share/ca-certificates/hypeman-egress-proxy.crt"
	if err := os.MkdirAll(filepath.Dir(caPath), 0755); err != nil {
		return fmt.Errorf("create CA directory: %w", err)
	}
	if err := os.WriteFile(caPath, []byte(cfg.EgressProxy.CACertPEM), 0644); err != nil {
		return fmt.Errorf("write proxy CA certificate: %w", err)
	}

	updateCACertificatesPath, err := lookupUpdateCACertificatesPath()
	if err != nil {
		log.Info("hypeman-init:egress-proxy", "installed egress proxy CA certificate, but update-ca-certificates was not found; guest trust store refresh skipped")
		return nil
	}

	cmd := exec.Command(updateCACertificatesPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("update-ca-certificates: %w", err)
	}
	log.Info("hypeman-init:egress-proxy", "installed egress proxy CA certificate and refreshed the guest trust store")
	return nil
}

func lookupUpdateCACertificatesPath() (string, error) {
	return exec.LookPath("update-ca-certificates")
}
