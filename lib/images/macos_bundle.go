package images

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxMacOSMetadataBytes = 64 << 10

// macOSMachinePayload is a complete cold-boot bundle, not a container rootfs.
// Admission of live storage and export sanitation belong to the caller.
type macOSMachinePayload struct {
	Disk     string
	Aux      string
	Platform *MacOSImage
}

// ValidateMacOSBundle validates a fixed-layout export, rejecting unknown metadata
// fields so an accidental credential field cannot become part of an image.
func ValidateMacOSBundle(root string) (*MacOSImage, error) {
	bundle, err := readMacOSMachineBundle(root, "disk.img", "aux.img", "config.json", true)
	if err != nil {
		return nil, err
	}
	return bundle.Platform, nil
}

func readMacOSMachineBundle(root, diskName, auxName, configName string, strict bool) (*macOSMachinePayload, error) {
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
	// Compare inodes: different hardlink names do not establish distinctness.
	if sameFile(disk, aux) || sameFile(disk, config) || sameFile(aux, config) {
		return nil, fmt.Errorf("machine bundle files must be distinct")
	}
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
	if strict {
		decoder.DisallowUnknownFields()
	}
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
	return &macOSMachinePayload{Disk: disk, Aux: aux, Platform: &platform}, nil
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

func sameFile(a, b string) bool {
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}
