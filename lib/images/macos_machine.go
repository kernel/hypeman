package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/paths"
)

const (
	MacOSMachineVersionLabel  = "io.hypeman.machine-image.version"
	MacOSMachineKindLabel     = "io.hypeman.machine-image.kind"
	MacOSMachineDiskLabel     = "io.hypeman.machine-image.disk-path"
	MacOSMachineFormatLabel   = "io.hypeman.machine-image.disk-format"
	MacOSMachineAuxLabel      = "io.hypeman.machine-image.aux-path"
	MacOSMachinePlatformLabel = "io.hypeman.machine-image.platform-path"
)

// stagedMacOS is the part of a staged macOS machine image beyond its base disk.
type stagedMacOS struct {
	layers   []string // staged overlays above the base, in order, beside their final paths
	aux      string   // staged auxiliary storage, beside its final path
	platform *MacOSImage
}

// finalDiskPath is where finalization installs the staged disk: the layout's rootfs
// path, or the raw machine disk beside it.
func (s stagedImageFiles) finalDiskPath(layout imageLayout) string {
	if s.macos != nil {
		return filepath.Join(layout.dir, machineDiskFile)
	}
	return layout.disk
}

// installMacOSMachine installs a macOS machine image's overlays and auxiliary
// storage and records its platform and chain length on meta. It returns the paths
// installed, for rollback. Rootfs images install nothing here.
func installMacOSMachine(layout imageLayout, staged stagedImageFiles, meta *imageMetadata, actualPlatform Platform) ([]string, error) {
	if actualPlatform.OS == "darwin" && staged.macos == nil {
		return nil, fmt.Errorf("macOS image requires a validated machine bundle")
	}
	if staged.macos == nil {
		return nil, nil
	}
	var installed []string
	for i, layer := range staged.macos.layers {
		path := filepath.Join(layout.dir, macOSDiskLayerFile(i+1))
		if err := os.Rename(layer, path); err != nil {
			return installed, err
		}
		installed = append(installed, path)
	}
	auxPath := filepath.Join(layout.dir, "aux.img")
	if err := os.Rename(staged.macos.aux, auxPath); err != nil {
		return installed, err
	}
	meta.MacOS = staged.macos.platform
	meta.MacOSDiskLayers = len(staged.macos.layers) + 1
	return append(installed, auxPath), nil
}

// checkCachedMachinePlatform refuses to reuse a record whose platform does not match
// an explicit request when either is darwin, so it answers the same way an uncached
// pull would. A ready record knows its platform; an in-flight pull knows only the
// platform it was requested as. Failed records are pulled again, so they pass.
func checkCachedMachinePlatform(p *paths.Paths, ref *ResolvedRef, requested Platform) error {
	meta, err := readMetadata(p, ref.Repository(), ref.DigestHex())
	if err != nil || meta.Status == StatusFailed {
		return nil
	}
	recorded := meta.Platform
	if meta.Status != StatusReady {
		recorded = ""
		if meta.Request != nil {
			recorded = meta.Request.Platform
		}
	}
	if recorded == "" {
		return nil
	}
	actual, err := ParsePlatform(recorded)
	if err == nil && requested.OS != "darwin" && actual.OS != "darwin" {
		return nil
	}
	if err != nil || !requested.Matches(actual) {
		return fmt.Errorf("%w: requested %s but cached manifest is %s", ErrInvalidPlatform, requested, recorded)
	}
	return nil
}

// stageMacOSOutputs stages a macOS machine image's boot disk chain and auxiliary
// storage in layoutDir, outside the manager lock. For other images it stages nothing
// and returns diskTemp for the rootfs export.
func (m *manager) stageMacOSOutputs(ctx context.Context, digest, root string, meta *containerMetadata, layoutDir, diskTemp, buildID string) (stagedImageFiles, error) {
	staged := stagedImageFiles{disk: diskTemp}
	if !isMacOSMachine(meta) {
		return staged, nil
	}
	start := time.Now()
	staged.disk = filepath.Join(layoutDir, machineDiskFile+".tmp-"+buildID)
	err := stageMacOSMachine(&staged, root, meta, layoutDir, buildID)
	m.recordImageBuildPhase(ctx, digest, "filesystem_export", time.Since(start), phaseStatus(err), "not_applicable")
	return staged, err
}

// readMacOSMachine reads the unpacked layers that validated labels describe.
func readMacOSMachine(root string, meta *containerMetadata) (*macOSMachinePayload, error) {
	labels := meta.Labels
	return readMacOSMachineLayers(root, labels[MacOSMachineDiskLabel], labels[MacOSMachineAuxLabel], labels[MacOSMachinePlatformLabel])
}

// stageMacOSMachine copies an unpacked image's disk chain and auxiliary storage to
// staging paths in layoutDir and checks that the chain stacks. The recorded size is
// the chain's logical size, as for a local import.
func stageMacOSMachine(staged *stagedImageFiles, root string, meta *containerMetadata, layoutDir, buildID string) error {
	payload, err := readMacOSMachine(root, meta)
	if err != nil {
		return err
	}
	staged.macos = &stagedMacOS{aux: filepath.Join(layoutDir, "aux.img.tmp-"+buildID), platform: payload.Platform}
	chain := []string{staged.disk}
	for i, disk := range payload.Disks {
		if i > 0 {
			layer := filepath.Join(layoutDir, macOSDiskLayerFile(i)+".tmp-"+buildID)
			staged.macos.layers = append(staged.macos.layers, layer)
			chain = append(chain, layer)
		}
		if _, err := stageMachineFile(disk, chain[i]); err != nil {
			return fmt.Errorf("stage boot disk layer %d: %w", i, err)
		}
	}
	if staged.sizeBytes, err = macOSChainSize(chain); err != nil {
		return fmt.Errorf("machine boot disk: %w", err)
	}
	if _, err := stageMachineFile(payload.Aux, staged.macos.aux); err != nil {
		return fmt.Errorf("stage auxiliary storage: %w", err)
	}
	return nil
}

func stageMachineFile(src, dst string) (int64, error) {
	// Copy helpers create the destination with the registry-supplied source mode,
	// and a multi-GB copy would expose contents before any later chmod. The copy
	// happens in installAtomically's fresh 0700 directory and is made 0600 before
	// it appears at the canonical staging path.
	err := installAtomically(dst, func(path string) error {
		if err := forkvm.CopyRegularFile(src, path); err != nil {
			return err
		}
		return os.Chmod(path, 0600)
	})
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// macOSMachineHost is true where macOS machine images can boot. Other hosts
// refuse to pull them. Tests override it.
var macOSMachineHost = runtime.GOOS == "darwin"

// checkMachinePlatformHost refuses a darwin image on a host that cannot boot it.
func checkMachinePlatformHost(platform Platform) error {
	if platform.OS == "darwin" && !macOSMachineHost {
		return fmt.Errorf("%w: macOS machine images can only be pulled on a macOS host", ErrInvalidPlatform)
	}
	return nil
}

// refuseMachineOnOtherHost refuses a darwin image before its layers are downloaded,
// on a host that cannot boot it. The registry client caches the config it fetches
// here for the layout write that follows.
func refuseMachineOnOtherHost(img gcr.Image) error {
	if macOSMachineHost {
		return nil
	}
	config, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("fetch image config: %w", ClassifyRegistryError(err))
	}
	return checkMachinePlatformHost(Platform{OS: config.OS, Architecture: config.Architecture, Variant: config.Variant}.Normalize())
}

// isMacOSMachine reports whether an image config is darwin. It normalizes as
// resolveManifestPlatform does, so aliases such as aarch64 and case variants such
// as Darwin select the machine path rather than rootfs.
func isMacOSMachine(meta *containerMetadata) bool {
	return Platform{OS: meta.OS}.Normalize().OS == "darwin"
}

// validateMacOSMachine checks a darwin config's host, platform and labels, so a
// malformed artifact fails before layer unpacking. The labeled files are checked
// when the unpacked bundle is read.
func validateMacOSMachine(meta *containerMetadata) error {
	normalized := Platform{OS: meta.OS, Architecture: meta.Architecture, Variant: meta.Variant}.Normalize()
	if err := checkMachinePlatformHost(normalized); err != nil {
		return err
	}
	if normalized.Architecture != "arm64" || normalized.Variant != "" {
		return fmt.Errorf("macOS machine requires darwin/arm64, not %s", normalized)
	}
	labels := meta.Labels
	for _, want := range [][2]string{{MacOSMachineVersionLabel, "2"}, {MacOSMachineKindLabel, "macos-image"}, {MacOSMachineFormatLabel, "asif"}} {
		if label := want[0]; labels[label] != want[1] {
			return fmt.Errorf("darwin artifact requires %s=%q, got %q; ordinary container images are not macOS machines", label, want[1], labels[label])
		}
	}
	pathLabels := []string{MacOSMachineDiskLabel, MacOSMachineAuxLabel, MacOSMachinePlatformLabel}
	for i, label := range pathLabels {
		path := labels[label]
		if path == "" || !filepath.IsLocal(path) {
			return fmt.Errorf("darwin artifact %s must be a local relative path, got %q", label, path)
		}
		for _, other := range pathLabels[:i] {
			if filepath.Clean(path) == filepath.Clean(labels[other]) {
				return fmt.Errorf("darwin artifact %s and %s name the same file %q", other, label, path)
			}
		}
	}
	return nil
}
