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

	cmd := exec.Command("/bin/sh", "-c", "if command -v update-ca-certificates >/dev/null 2>&1; then update-ca-certificates >/dev/null 2>&1; fi")
	if err := cmd.Run(); err != nil {
		log.Error("hypeman-init:egress-proxy", "failed to run update-ca-certificates", err)
		return
	}
	log.Info("hypeman-init:egress-proxy", "installed egress proxy CA certificate")
}
