package builds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
)

type machineExportManager interface {
	StopMacOSBuildInstance(context.Context, string) (instances.MacOSBuildExportReceipt, error)
	ExportStoppedMacOSBuildInstance(context.Context, string, string) error
}

// GuestMachineBuildDriver uses ordinary instance lifecycle and the shared system
// GuestService. It never installs an agent or silently upgrades an unmanaged base.
type GuestMachineBuildDriver struct {
	Instances instances.Manager
	WorkDir   string
}

func (d *GuestMachineBuildDriver) Start(ctx context.Context, name string, base *images.Image, policy BuildPolicy) (MachineBuildSession, error) {
	exporter, ok := d.Instances.(machineExportManager)
	if !ok || !filepath.IsAbs(d.WorkDir) {
		return nil, fmt.Errorf("strict machine export and private workspace required")
	}
	if _, ok = ctx.Deadline(); !ok {
		return nil, fmt.Errorf("machine start requires a deadline")
	}
	if base == nil || base.Status != images.StatusReady || base.Platform != "darwin/arm64" || base.MacOS == nil || !base.MacOS.GuestAgent || base.MacOS.Validate() != nil || !ValidMachineDigest(base.Digest) {
		return nil, fmt.Errorf("provisioned pinned macOS base required")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if err := normalizeMachinePolicy(&policy); err != nil {
		return nil, err
	}
	if policy.CPUs != int(base.MacOS.CPUs) || policy.MemoryMB != int(base.MacOS.Memory>>20) {
		return nil, fmt.Errorf("machine policy must preserve resources and supported networking")
	}
	ref, err := images.ParseNormalizedRef(base.Name)
	if err != nil {
		return nil, err
	}
	// Workspaces are named per builder, so one left by an interrupted attempt
	// of this job is removed here.
	prefix := "guest-machine-" + name + "-"
	stale, _ := filepath.Glob(filepath.Join(d.WorkDir, prefix+"*"))
	for _, dir := range stale {
		_ = os.RemoveAll(dir)
	}
	dir, err := os.MkdirTemp(d.WorkDir, prefix)
	if err != nil {
		return nil, err
	}
	inst, err := d.Instances.CreateInstance(ctx, instances.CreateInstanceRequest{Name: name, Image: ref.Repository() + "@" + base.Digest, Platform: "darwin/arm64", Hypervisor: hypervisor.TypeVZ, Size: int64(base.MacOS.Memory), Vcpus: int(base.MacOS.CPUs), NetworkEnabled: policy.NetworkMode == "egress"})
	if inst == nil {
		_ = os.RemoveAll(dir)
		if err == nil {
			err = fmt.Errorf("instance creation returned no instance")
		}
		return nil, err
	}
	session := &guestMachineSession{manager: d.Instances, exporter: exporter, instance: inst, workspace: dir, guestDir: "/var/root/hypeman-build-" + inst.Id}
	return session, err
}

type guestMachineSession struct {
	manager             instances.Manager
	exporter            machineExportManager
	instance            *instances.Instance
	workspace, guestDir string
	receipt             MachineStopReceipt
	sanitized           bool
}

func (s *guestMachineSession) ID() string { return s.instance.Id }
func (s *guestMachineSession) dialer() (hypervisor.VsockDialer, error) {
	return hypervisor.NewVsockDialer(s.instance.HypervisorType, s.instance.VsockSocket, s.instance.VsockCID)
}
func (s *guestMachineSession) exec(ctx context.Context, command []string, cwd string, env map[string]string, wait time.Duration) error {
	dialer, err := s.dialer()
	if err != nil {
		return err
	}
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{Command: command, Cwd: cwd, Env: env, WaitForAgent: wait})
	if err != nil {
		return err
	}
	if exit == nil {
		return fmt.Errorf("machine guest command did not succeed")
	}
	if exit.Code != 0 {
		return &machineExitError{code: exit.Code}
	}
	return nil
}

// machineExitError is a guest command's nonzero exit code.
type machineExitError struct{ code int }

func (e *machineExitError) Error() string { return "machine guest command did not succeed" }

func (s *guestMachineSession) Provision(ctx context.Context, source string) error {
	root := filepath.Join(s.workspace, "source")
	recipe, err := extractMachineRecipe(ctx, source, root)
	if err != nil {
		return err
	}
	if err = s.exec(ctx, []string{"/bin/sh", "-c", `test "$(/usr/bin/id -u)" -eq 0`}, "", nil, 30*time.Second); err != nil {
		return err
	}
	if err = s.exec(ctx, []string{"/bin/test", "!", "-e", s.guestDir}, "", nil, 30*time.Second); err != nil {
		return err
	}
	if err = s.exec(ctx, []string{"/bin/mkdir", "-m", "700", s.guestDir}, "", nil, 0); err != nil {
		return err
	}
	dialer, err := s.dialer()
	if err != nil {
		return err
	}
	if err = guest.CopyToInstance(ctx, dialer, guest.CopyToInstanceOptions{SrcPath: root, DstPath: s.guestDir}); err != nil {
		return err
	}
	for i, step := range recipe.Steps {
		if err = s.exec(ctx, step.Command, s.guestDir, step.Env, 0); err != nil {
			if exitErr := (*machineExitError)(nil); errors.As(err, &exitErr) {
				return &machineStepError{step: i + 1, code: exitErr.code}
			}
			return err
		}
	}
	return nil
}

func (s *guestMachineSession) Sanitize(ctx context.Context) error {
	if err := s.exec(ctx, []string{"/bin/rm", "-rf", "--", s.guestDir}, "", nil, 0); err != nil {
		return err
	}
	if err := s.exec(ctx, []string{"/bin/test", "!", "-e", s.guestDir}, "", nil, 0); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.workspace, "source")); err != nil {
		return err
	}
	s.sanitized = true
	return nil
}

// Stop is the publication path: only an acknowledged graceful power-off with
// confirmed VMM exit may produce a receipt that permits export.
func (s *guestMachineSession) Stop(ctx context.Context) (MachineStopReceipt, error) {
	if s.receipt.VMMExited {
		return s.receipt, nil
	}
	receipt, err := s.exporter.StopMacOSBuildInstance(ctx, s.ID())
	s.receipt = receipt
	return s.receipt, err
}

// Abort is failure cleanup only. Nothing from a failed build is exported, so after
// a strict attempt it may use the ordinary forced hypervisor stop. Destroy still
// requires confirmed VMM exit; a non-exited instance remains quarantined.
func (s *guestMachineSession) Abort(ctx context.Context) (MachineStopReceipt, error) {
	if s.receipt.VMMExited {
		return s.receipt, nil
	}
	if receipt, err := s.exporter.StopMacOSBuildInstance(ctx, s.ID()); err == nil && receipt.VMMExited {
		s.receipt = receipt
		return s.receipt, nil
	}
	if inst, err := s.manager.GetInstance(ctx, s.ID()); err != nil || inst.State != instances.StateStopped {
		inst, err = s.manager.StopInstance(ctx, s.ID())
		if err != nil {
			return s.receipt, err
		}
		if inst == nil || inst.State != instances.StateStopped {
			return s.receipt, fmt.Errorf("forced stop did not confirm VMM exit")
		}
	}
	s.receipt = MachineStopReceipt{VMMExited: true}
	return s.receipt, nil
}

// Export copies only from closed storage of a Stopped instance, under the
// instance lock, after a strict graceful receipt and sanitation.
func (s *guestMachineSession) Export(ctx context.Context, destination string) error {
	if !s.receipt.Graceful || !s.receipt.VMMExited || !s.sanitized {
		return fmt.Errorf("sanitized strict stop receipt required")
	}
	return s.exporter.ExportStoppedMacOSBuildInstance(ctx, s.ID(), destination)
}

func (s *guestMachineSession) Destroy(ctx context.Context) error {
	if !s.receipt.VMMExited {
		return fmt.Errorf("unconfirmed VMM exit: instance quarantined")
	}
	if err := s.manager.DeleteInstance(ctx, s.ID()); err != nil {
		return err
	}
	return os.RemoveAll(s.workspace)
}
