//go:build linux

package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type dmLinearDevice struct {
	name  string
	loops []string
	once  sync.Once
	err   error
}

// createDMLinearDevice exposes regular layer artifacts as one read-only block
// device. Loop and device-mapper state is host-local and must be released with
// Close when no VM has the device open.
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
			_ = device.Close()
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

	tableFile, err := os.CreateTemp("", "hypeman-dm-table-*")
	if err != nil {
		return nil, fmt.Errorf("create device-mapper table: %w", err)
	}
	tablePath := tableFile.Name()
	defer os.Remove(tablePath)
	if _, err := tableFile.WriteString(strings.Join(table, "\n") + "\n"); err != nil {
		tableFile.Close()
		return nil, fmt.Errorf("write device-mapper table: %w", err)
	}
	if err := tableFile.Close(); err != nil {
		return nil, fmt.Errorf("close device-mapper table: %w", err)
	}
	if _, err := runCommand(ctx, "dmsetup", "create", "--readonly", "--noudevsync", name, tablePath); err != nil {
		return nil, fmt.Errorf("create device-mapper device %s: %w", name, err)
	}

	cleanup = false
	return device, nil
}

func (d *dmLinearDevice) Path() string {
	return filepath.Join("/dev/mapper", d.name)
}

func (d *dmLinearDevice) Close() error {
	d.once.Do(func() {
		if _, err := runCommand(context.Background(), "dmsetup", "remove", "--retry", "--noudevsync", d.name); err != nil && !strings.Contains(err.Error(), "No such device") {
			d.err = fmt.Errorf("remove device-mapper device %s: %w", d.name, err)
		}
		for i := len(d.loops) - 1; i >= 0; i-- {
			if _, err := runCommand(context.Background(), "losetup", "--detach", d.loops[i]); err != nil && d.err == nil {
				d.err = fmt.Errorf("detach loop device %s: %w", d.loops[i], err)
			}
		}
	})
	return d.err
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
