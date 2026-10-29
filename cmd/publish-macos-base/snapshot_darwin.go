//go:build darwin && arm64

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
)

func publish(ctx context.Context, registry, secret, id, bundle string) (string, string, error) {
	publisher := builds.StreamingMachinePublisher{RegistryURL: registry, Tokens: builds.NewRegistryTokenGenerator(secret)}
	result, err := publisher.Publish(ctx, id, bundle)
	if err != nil {
		return "", "", err
	}
	return result.Reference, result.Digest, nil
}

func snapshot(ctx context.Context, source, workspace string) (string, error) {
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("root required to inspect all open storage")
	}
	p := paths.New(filepath.Join(workspace, "data"))
	// Reuse the offline import's lsof fail-closed check and APFS clones. The
	// operator must still exclude restart/writers for the whole snapshot window.
	img, err := images.ImportMacOSImage(ctx, p, "localhost/macos-base:export", source)
	if err != nil {
		return "", err
	}
	ref, err := images.ParseNormalizedRef(img.Name)
	if err != nil {
		return "", err
	}
	imported := p.ImageDigestDir(ref.Repository(), img.Digest[7:])
	bundle := filepath.Join(workspace, "bundle")
	if err = os.Mkdir(bundle, 0700); err != nil {
		return "", err
	}
	for src, dst := range map[string]string{"rootfs.raw": "disk.img", "aux.img": "aux.img"} {
		if err = os.Rename(filepath.Join(imported, src), filepath.Join(bundle, dst)); err != nil {
			return "", err
		}
	}
	// Canonical metadata from import; do not carry arbitrary host bundle files.
	data, err := json.Marshal(img.MacOS)
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
		return "", err
	}
	return bundle, nil
}
