//go:build linux

package devices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// nvidiaSMITimeout bounds the creatable-placements query, which takes about
// 0.15 s on an L40S host. With every host core and the GPU saturated it took
// up to 0.55 s, and 2.3 s with 20 running at once.
var nvidiaSMITimeout = 5 * time.Second

var errNvidiaSMIStuck = errors.New("an earlier nvidia-smi run is still running after its timeout")

// nvidiaSMIStuck counts timed-out nvidia-smi runs that have not returned.
var nvidiaSMIStuck atomic.Int32

// nvidiaSMIFallbacks counts creatable-placement queries answered from the
// sysfs lists instead, by reason.
var nvidiaSMIFallbacks = map[string]*atomic.Int64{
	"error":   {},
	"timeout": {},
	"stuck":   {},
}

// NvidiaSMIFallbacks returns how many creatable-placement queries have
// fallen back to the sysfs lists since startup, by reason: nvidia-smi
// failed (error), timed out (timeout), or an earlier timed-out run had not
// returned yet (stuck).
func NvidiaSMIFallbacks() map[string]int64 {
	counts := make(map[string]int64, len(nvidiaSMIFallbacks))
	for reason, count := range nvidiaSMIFallbacks {
		counts[reason] = count.Load()
	}
	return counts
}

func recordNvidiaSMIFallback(err error) {
	reason := "error"
	switch {
	case errors.Is(err, errNvidiaSMIStuck):
		reason = "stuck"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	}
	nvidiaSMIFallbacks[reason].Add(1)
	slog.Default().Warn("falling back to sysfs creatable vGPU types", "reason", reason, "error", err)
}

// creatableProfile is a profile a GPU can still create, with the placement
// IDs a new vGPU of that profile could start at.
type creatableProfile struct {
	VGPUProfileType
	placements []int
}

// creatableProfiles asks the driver which profiles each GPU can still create
// and at which placements (nvidia-smi vgpu -c -v), keyed by parent GPU
// address. Unlike the per-VF creatable_vgpu_types lists, this stays accurate
// on a busy GPU in mixed-size mode; see GPU.md. It fails if the GPU of any VF
// is missing from the output, so a GPU the driver did not report is never
// mistaken for a full one.
func (s vendorVFIOSysfs) creatableProfiles(vfs []VirtualFunction) (map[string][]creatableProfile, error) {
	run := s.nvidiaSMIFunc
	if run == nil {
		run = runNvidiaSMI
	}
	out, err := runBounded(run, "vgpu", "-c", "-v")
	if err != nil {
		return nil, fmt.Errorf("query creatable vGPU placements: %w", err)
	}
	byGPU, err := parseCreatableProfiles(string(out))
	if err != nil {
		return nil, err
	}
	for _, vf := range vfs {
		if _, ok := byGPU[vf.ParentGPU]; !ok {
			return nil, fmt.Errorf("nvidia-smi did not report GPU %q of VF %s", vf.ParentGPU, vf.PCIAddress)
		}
	}
	return byGPU, nil
}

// runBounded returns at nvidiaSMITimeout even if run has not: a process stuck
// in the driver can outlive SIGKILL. Until such a run returns, later calls
// fail at once instead of each waiting out the timeout, which the claim
// would pay while holding the vGPU allocation lock.
func runBounded(run func(context.Context, ...string) ([]byte, error), args ...string) ([]byte, error) {
	if nvidiaSMIStuck.Load() > 0 {
		return nil, errNvidiaSMIStuck
	}
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSMITimeout)
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := run(ctx, args...)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
	}
	nvidiaSMIStuck.Add(1)
	go func() {
		<-done
		nvidiaSMIStuck.Add(-1)
	}()
	return nil, fmt.Errorf("nvidia-smi %s: %w", strings.Join(args, " "), ctx.Err())
}

func runNvidiaSMI(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "nvidia-smi", args...).Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("nvidia-smi %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// parseCreatableProfiles parses nvidia-smi vgpu -c -v output. Each GPU
// starts with an unindented "GPU <bus ID>" line. A GPU that cannot create
// any profile has only a "No vGPUs found on this device" line and maps to an
// empty list.
func parseCreatableProfiles(output string) (map[string][]creatableProfile, error) {
	byGPU := make(map[string][]creatableProfile)
	gpu := ""
	var current *creatableProfile
	finish := func() error {
		if current == nil {
			return nil
		}
		if current.Name == "" || len(current.placements) == 0 {
			return fmt.Errorf("vGPU type %s on GPU %s has no name or placement IDs", current.TypeName, gpu)
		}
		byGPU[gpu] = append(byGPU[gpu], *current)
		current = nil
		return nil
	}
	for _, line := range strings.Split(output, "\n") {
		// Type fields such as "GPU Instance Profile ID" also start with
		// "GPU", but are indented.
		if busID, ok := strings.CutPrefix(line, "GPU "); ok {
			if err := finish(); err != nil {
				return nil, err
			}
			address, err := sysfsPCIAddress(strings.TrimSpace(busID))
			if err != nil {
				return nil, err
			}
			gpu = address
			byGPU[gpu] = []creatableProfile{}
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "vGPU Type ID":
			if err := finish(); err != nil {
				return nil, err
			}
			if gpu == "" {
				return nil, fmt.Errorf("vGPU type %s listed before any GPU", value)
			}
			id, err := strconv.ParseUint(value, 0, 32)
			if err != nil {
				return nil, fmt.Errorf("parse vGPU type ID %q: %w", value, err)
			}
			current = &creatableProfile{VGPUProfileType: VGPUProfileType{TypeName: strconv.FormatUint(id, 10)}}
		case "Name":
			if current != nil {
				current.Name = value
				current.FramebufferMB = framebufferFromProfileName(value)
			}
		case "Placement IDs":
			if current == nil {
				continue
			}
			for _, field := range strings.Fields(value) {
				id, err := strconv.Atoi(field)
				if err != nil {
					return nil, fmt.Errorf("parse placement IDs %q of vGPU type %s on GPU %s: %w", value, current.TypeName, gpu, err)
				}
				current.placements = append(current.placements, id)
			}
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if len(byGPU) == 0 {
		return nil, fmt.Errorf("no GPUs in nvidia-smi output")
	}
	return byGPU, nil
}

// sysfsPCIAddress converts an nvidia-smi bus ID such as 00000000:E3:00.0 to
// the sysfs form 0000:e3:00.0.
func sysfsPCIAddress(busID string) (string, error) {
	domain, rest, ok := strings.Cut(busID, ":")
	if !ok {
		return "", fmt.Errorf("parse GPU bus ID %q", busID)
	}
	value, err := strconv.ParseUint(domain, 16, 32)
	if err != nil {
		return "", fmt.Errorf("parse GPU bus ID %q: %w", busID, err)
	}
	return fmt.Sprintf("%04x:%s", value, strings.ToLower(rest)), nil
}
