//go:build linux

package images

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type dmLinearDevice struct {
	name   string
	loops  []string
	ownsDM bool
}

// createDMLinearDevice exposes regular layer artifacts as one read-only block
// device. Loop and device-mapper state is host-local and must be released with
// Close when no VM has the device open.
func dmLinearAvailable(ctx context.Context) bool {
	_, err := runCommand(ctx, "dmsetup", "targets")
	return err == nil
}

func existingDMLinearDevice(ctx context.Context, name string, backingPathCount int) (*dmLinearDevice, bool, error) {
	if _, err := runCommand(ctx, "dmsetup", "info", "--noheadings", "--columns", "-o", "name", name); err != nil {
		if isDMDeviceMissing(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	output, err := runCommand(ctx, "dmsetup", "deps", "--noheadings", "--separator=,", "-o", "devname", name)
	if err != nil {
		return nil, false, fmt.Errorf("inspect device-mapper dependencies for %s: %w", name, err)
	}
	capacity := backingPathCount
	if capacity < 0 {
		capacity = 0
	}
	loops := parseDMLinearDependencies(output, capacity)
	if backingPathCount >= 0 && len(loops) != backingPathCount {
		return nil, false, fmt.Errorf("device-mapper device %s has %d loop dependencies, want %d", name, len(loops), backingPathCount)
	}
	return &dmLinearDevice{name: name, loops: loops, ownsDM: true}, true, nil
}

func createDMLinearDevice(ctx context.Context, name string, backingPaths []string) (*dmLinearDevice, error) {
	if err := validateDMName(name); err != nil {
		return nil, err
	}
	if len(backingPaths) == 0 {
		return nil, errors.New("device-mapper requires at least one backing path")
	}

	device := &dmLinearDevice{name: name}
	cleanup := true
	defer func() {
		if cleanup {
			_ = device.Close(context.Background())
		}
	}()

	for _, path := range backingPaths {
		loop, err := runCommand(ctx, "losetup", "--read-only", "--find", "--show", path)
		if err != nil {
			return nil, fmt.Errorf("attach read-only loop device for %s: %w", path, err)
		}
		loop = strings.TrimSpace(loop)
		if loop == "" {
			return nil, fmt.Errorf("attach read-only loop device for %s returned no device", path)
		}
		device.loops = append(device.loops, loop)
	}

	table := make([]string, 0, len(device.loops))
	var offset int64
	for _, loop := range device.loops {
		sectorsText, err := runCommand(ctx, "blockdev", "--getsz", loop)
		if err != nil {
			return nil, fmt.Errorf("get size of %s: %w", loop, err)
		}
		sectors, err := strconv.ParseInt(strings.TrimSpace(sectorsText), 10, 64)
		if err != nil || sectors <= 0 {
			return nil, fmt.Errorf("invalid size for %s: %q", loop, strings.TrimSpace(sectorsText))
		}
		table = append(table, fmt.Sprintf("%d %d linear %s 0", offset, sectors, loop))
		offset += sectors
	}

	if _, err := runCommand(ctx, "dmsetup", "create", "--readonly", "--noudevsync", name, "--table", strings.Join(table, "\n")); err != nil {
		return nil, fmt.Errorf("create device-mapper device %s: %w", name, err)
	}
	device.ownsDM = true

	cleanup = false
	return device, nil
}

func (d *dmLinearDevice) Path() string {
	return filepath.Join("/dev/mapper", d.name)
}

func listDMDeviceNames(ctx context.Context, prefix string) ([]string, error) {
	output, err := runCommand(ctx, "dmsetup", "ls", "--noheadings", "--columns", "-o", "name")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errFsmergeUnsupported
		}
		return nil, err
	}
	names := make([]string, 0)
	for _, line := range strings.Split(output, "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

func parseDMLinearDependencies(output string, capacity int) []string {
	loops := make([]string, 0, capacity)
	for _, token := range strings.Fields(output) {
		loop := strings.Trim(token, "(),")
		if strings.HasPrefix(loop, "loop") {
			loops = append(loops, filepath.Join("/dev", loop))
		}
	}
	return loops
}

func isDMDeviceMissing(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "No such device") || strings.Contains(message, "does not exist") || strings.Contains(message, "not found")
}

func (d *dmLinearDevice) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.ownsDM {
		if _, err := runCommand(ctx, "dmsetup", "remove", "--retry", "--noudevsync", d.name); err != nil && !isDMDeviceMissing(err) {
			return fmt.Errorf("remove device-mapper device %s: %w", d.name, err)
		}
		d.ownsDM = false
	}

	var closeErr error
	for i := len(d.loops) - 1; i >= 0; i-- {
		loop := d.loops[i]
		if _, err := runCommand(ctx, "losetup", "--detach", loop); err != nil && !isDMDeviceMissing(err) {
			if closeErr == nil {
				closeErr = fmt.Errorf("detach loop device %s: %w", loop, err)
			}
			continue
		}
		d.loops = append(d.loops[:i], d.loops[i+1:]...)
	}
	return closeErr
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func validateDMName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid device-mapper name %q", name)
	}
	return nil
}
