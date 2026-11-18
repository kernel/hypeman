package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const maxMacOSMetadataBytes = 64 << 10

// macOSMachinePayload is a complete cold-boot bundle, not a container rootfs: an
// ASIF boot disk chain, base first, with the auxiliary storage and platform of its
// top layer. Admission of live storage and export sanitation belong to the caller.
type macOSMachinePayload struct {
	Disks    []string
	Aux      string
	Platform *MacOSImage
}

// RequireClosedFile fails unless no process holds path open. lsof exits 1 when
// nothing holds the file; any other failure leaves the storage unverified.
func RequireClosedFile(ctx context.Context, path string) error {
	err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-t", path).Run()
	if err == nil {
		return fmt.Errorf("storage is open: stop its VM first")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		return fmt.Errorf("cannot verify storage is closed: %w", err)
	}
	return nil
}

func readMacOSMachineBundle(root, diskName, auxName, configName string) (*macOSMachinePayload, error) {
	disk, err := machineBundleFile(root, diskName)
	if err != nil {
		return nil, err
	}
	aux, err := machineBundleFile(root, auxName)
	if err != nil {
		return nil, err
	}
	config, err := machineBundleFile(root, configName)
	if err != nil {
		return nil, err
	}
	if err := CheckMachineFiles(disk, aux, config); err != nil {
		return nil, err
	}
	platform, err := readMacOSPlatform(config)
	if err != nil {
		return nil, err
	}
	return &macOSMachinePayload{Disks: []string{disk}, Aux: aux, Platform: platform}, nil
}

// readMacOSMachineLayers reads a pulled image unpacked one layer per numbered
// directory. Every layer holds a disk; the auxiliary storage and platform come from
// the highest layer that has them.
func readMacOSMachineLayers(root, diskName, auxName, configName string) (*macOSMachinePayload, error) {
	var payload macOSMachinePayload
	var top string
	for i := 0; ; i++ {
		dir := filepath.Join(root, strconv.Itoa(i))
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			break
		}
		disk, err := machineBundleFile(dir, diskName)
		if err != nil {
			return nil, fmt.Errorf("machine layer %d: %w", i, err)
		}
		payload.Disks = append(payload.Disks, disk)
		top = dir
	}
	if len(payload.Disks) == 0 {
		return nil, fmt.Errorf("machine image needs a disk in every layer")
	}
	// Only the top layer's auxiliary storage and platform count, so a lower layer
	// can be reused from another image without unpacking it.
	aux, err := machineBundleFile(top, auxName)
	if err != nil {
		return nil, fmt.Errorf("machine image top layer: %w", err)
	}
	payload.Aux = aux
	config, err := machineBundleFile(top, configName)
	if err != nil {
		return nil, fmt.Errorf("machine image top layer: %w", err)
	}
	if err := CheckMachineFiles(append([]string{aux, config}, payload.Disks...)...); err != nil {
		return nil, err
	}
	platform, err := readMacOSPlatform(config)
	if err != nil {
		return nil, err
	}
	payload.Platform = platform
	return &payload, nil
}

func readMacOSPlatform(config string) (*MacOSImage, error) {
	file, err := os.Open(config)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMacOSMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMacOSMetadataBytes {
		return nil, fmt.Errorf("machine platform metadata is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var platform MacOSImage
	if err := decoder.Decode(&platform); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("machine platform metadata must contain one JSON object")
	}
	if err := platform.Validate(); err != nil {
		return nil, err
	}
	return &platform, nil
}

func machineBundleFile(root, relative string) (string, error) {
	if relative == "" || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("machine payload path must be local and relative")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("machine payload escapes artifact root")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("machine payload must be a nonempty regular file")
	}
	return path, nil
}

// CheckMachineFiles requires each path to be a nonempty regular file, not a
// symlink, and no two to be the same file. Inodes are compared: different hardlink
// names do not establish distinctness.
func CheckMachineFiles(paths ...string) error {
	seen := make([]os.FileInfo, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("machine storage must be nonempty regular files")
		}
		for _, other := range seen {
			if os.SameFile(other, info) {
				return fmt.Errorf("machine storage files must be distinct")
			}
		}
		seen = append(seen, info)
	}
	return nil
}
