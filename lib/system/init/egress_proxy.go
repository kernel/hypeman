package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kernel/hypeman/lib/vmconfig"
)

func installEgressProxyCA(log *Logger, cfg *vmconfig.Config) {
	if cfg == nil || cfg.EgressProxy == nil || !cfg.EgressProxy.Enabled || cfg.EgressProxy.CACertPEM == "" {
		return
	}

	caPath := "/usr/local/share/ca-certificates/hypeman-egress-proxy.crt"
	if err := os.MkdirAll(filepath.Dir(caPath), 0755); err != nil {
		log.Error("hypeman-init:egress-proxy", "failed to create CA directory", err)
		return
	}
	if err := os.WriteFile(caPath, []byte(cfg.EgressProxy.CACertPEM), 0644); err != nil {
		log.Error("hypeman-init:egress-proxy", "failed to write proxy CA", err)
		return
	}

	updateCACertificatesPath, err := lookupUpdateCACertificatesPath()
	if err != nil {
		log.Info("hypeman-init:egress-proxy", "installed egress proxy CA certificate, but update-ca-certificates was not found; guest trust store refresh skipped")
		return
	}

	cmd := exec.Command(updateCACertificatesPath)
	if err := cmd.Run(); err != nil {
		log.Error("hypeman-init:egress-proxy", "failed to run update-ca-certificates", err)
		return
	}
	log.Info("hypeman-init:egress-proxy", "installed egress proxy CA certificate and refreshed the guest trust store")
}

func lookupUpdateCACertificatesPath() (string, error) {
	return exec.LookPath("update-ca-certificates")
}
