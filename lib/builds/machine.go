package builds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/images"
)

// MachineBuildRequest is an internal backend contract, not a new recipe syntax
// or an HTTP API. The normal build manager owns ID, source staging and publish
// authorization. A concrete driver must require a provisioned system agent.
type MachineBuildRequest struct {
	ID         string
	BaseImage  string      // Installed darwin/arm64 image, pinned by sha256 digest.
	SourceHash string      // Optional expected lowercase SHA256 of the source archive.
	Policy     BuildPolicy // Zero CPU/memory inherit the base machine's exact resources.
}

type MachineStopReceipt struct{ Graceful, VMMExited bool }
type MachinePublication struct{ Reference, Digest string }
type MachineBuildResult struct {
	Publication       MachinePublication
	Provenance        BuildProvenance
	BuilderInstanceID string
}

type MachineBuildDriver interface {
	// Start owns an isolated instance and identity lease. A partially created
	// session must be returned even with an error so cleanup can stop it.
	Start(context.Context, *images.Image, BuildPolicy) (MachineBuildSession, error)
}

type MachineBuildSession interface {
	ID() string
	Provision(context.Context, string) error // Build-owned staged source, never a client host path.
	Sanitize(context.Context) error          // Remove build credentials, source and secret injection artifacts.
	// Stop must await actual VMM exit. Forced-stop fallback must not claim graceful.
	Stop(context.Context) (MachineStopReceipt, error)
	Export(context.Context, string) error // Stopped disk.img, aux.img, config.json only.
	// Destroy removes instance storage only after proven VMM exit, not the export.
	Destroy(context.Context) error
}

type MachineBuildPublisher interface {
	// Publisher owns registry address, scoped token and builds/{id} destination.
	// It must stream blobs and commit/verify the manifest last. An ambiguous
	// network error is not a verified successful publication.
	Publish(context.Context, string, string) (MachinePublication, error)
}

type MachineBuildBackend struct {
	Driver    MachineBuildDriver
	Publisher MachineBuildPublisher
}

// machinePhaseError keeps potentially secret-bearing guest/command errors out of
// ordinary log/error text, while retaining errors.Is/As for internal handling.
type machinePhaseError struct {
	phase string
	cause error
}

func (e *machinePhaseError) Error() string         { return "machine build " + e.phase + " failed" }
func (e *machinePhaseError) Unwrap() error         { return e.cause }
func machineFailure(phase string, err error) error { return &machinePhaseError{phase, err} }

// runPrepared executes only machine-specific phases. The shared build manager
// owns resolution, source staging/hash verification, deadline, queue and status.
func (r *MachineBuildBackend) runPrepared(ctx context.Context, req MachineBuildRequest, base *images.Image, sourcePath, workDir string) (result *MachineBuildResult, err error) {
	if r.Driver == nil || r.Publisher == nil || !filepath.IsAbs(workDir) || !filepath.IsAbs(sourcePath) || len(req.SourceHash) != 64 || strings.Trim(req.SourceHash, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("machine driver, publisher, private workspace and verified source required")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, fmt.Errorf("prepared machine job requires a deadline")
	}
	if len(req.ID) == 0 || len(req.ID) > 128 || strings.Trim(req.ID, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return nil, fmt.Errorf("invalid machine build ID")
	}
	ref, e := images.ParseNormalizedRef(req.BaseImage)
	if e != nil || !ref.IsDigest() || !machineDigest(ref.Digest()) {
		return nil, fmt.Errorf("machine base must be pinned by sha256 digest")
	}
	policy := req.Policy
	if policy.TimeoutSeconds < 0 {
		return nil, fmt.Errorf("invalid machine build timeout")
	}
	if policy.TimeoutSeconds == 0 {
		policy.TimeoutSeconds = 600
	}
	if policy.TimeoutSeconds > 86400 {
		return nil, fmt.Errorf("machine build timeout exceeds 24 hours")
	}
	if len(policy.AllowedDomains) != 0 {
		return nil, fmt.Errorf("macOS domain-restricted egress is unsupported")
	}
	if policy.NetworkMode == "" {
		policy.NetworkMode = "isolated"
	}
	if policy.NetworkMode != "isolated" && policy.NetworkMode != "egress" {
		return nil, fmt.Errorf("unsupported machine network mode")
	}
	if base == nil || base.Status != images.StatusReady || base.Platform != "darwin/arm64" || base.MacOS == nil || base.Digest != ref.Digest() || base.MacOS.CPUs < 2 || base.MacOS.CPUs > MaxBuildCPUs || base.MacOS.Memory < 4<<30 || base.MacOS.Memory > uint64(MaxBuildMemoryMB)<<20 || base.MacOS.Memory%(1<<20) != 0 {
		return nil, fmt.Errorf("base is not the pinned ready macOS machine")
	}
	if base.MacOS.Validate() != nil {
		return nil, fmt.Errorf("base has invalid machine identity")
	}
	memory := int(base.MacOS.Memory >> 20)
	cpus := int(base.MacOS.CPUs)
	if (policy.MemoryMB != 0 && policy.MemoryMB != memory) || (policy.CPUs != 0 && policy.CPUs != cpus) {
		return nil, fmt.Errorf("machine resources must match installed base")
	}
	policy.MemoryMB = memory
	policy.CPUs = cpus
	if e = policy.Validate(); e != nil {
		return nil, e
	}
	workspace, e := os.MkdirTemp(workDir, "machine-build-")
	if e != nil {
		return nil, machineFailure("workspace", e)
	}
	defer os.RemoveAll(workspace)
	if e = verifyBuildSource(ctx, sourcePath, req.SourceHash); e != nil {
		return nil, machineFailure("verify_source", e)
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	session, e := r.Driver.Start(ctx, base, policy)
	if session == nil {
		if e == nil {
			e = fmt.Errorf("driver returned no session")
		}
		return nil, machineFailure("start", e)
	}
	stopped, destroyed := false, false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if !stopped {
			receipt, stopErr := session.Stop(cleanup)
			stopped = receipt.VMMExited
			if !stopped {
				err = errors.Join(err, machineFailure("quarantine_unconfirmed_vmm_exit", stopErr))
				result = nil
				return
			}
		}
		if !destroyed {
			if destroyErr := session.Destroy(cleanup); destroyErr != nil {
				err = errors.Join(err, machineFailure("cleanup", destroyErr))
				result = nil
			}
		}
	}()
	if e != nil {
		return nil, machineFailure("start", e)
	}
	instanceID := session.ID()
	if e = session.Provision(ctx, sourcePath); e != nil {
		return nil, machineFailure("provision", e)
	}
	if e = session.Sanitize(ctx); e != nil {
		return nil, machineFailure("sanitize", e)
	}
	receipt, e := session.Stop(ctx)
	stopped = receipt.VMMExited
	if e != nil || !receipt.Graceful || !stopped {
		return nil, machineFailure("graceful_stop", e)
	}
	bundle := filepath.Join(workspace, "bundle")
	if e = os.Mkdir(bundle, 0700); e != nil {
		return nil, machineFailure("export", e)
	}
	if e = session.Export(ctx, bundle); e != nil {
		return nil, machineFailure("export", e)
	}
	if e = validateMachineExport(bundle, base.MacOS); e != nil {
		return nil, machineFailure("validate_export", e)
	}
	if e = session.Destroy(ctx); e != nil {
		return nil, machineFailure("cleanup", e)
	}
	destroyed = true
	if e = os.Remove(sourcePath); e != nil {
		return nil, machineFailure("cleanup_source", e)
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	publication, e := r.Publisher.Publish(ctx, req.ID, bundle)
	if e != nil {
		return nil, machineFailure("publish", e)
	}
	output, e := images.ParseNormalizedRef(publication.Reference)
	if e != nil || !output.IsDigest() || !machineDigest(publication.Digest) || output.Digest() != publication.Digest {
		return nil, fmt.Errorf("machine publication receipt is unverified")
	}
	return &MachineBuildResult{Publication: publication, BuilderInstanceID: instanceID, Provenance: BuildProvenance{BaseImageDigest: base.Digest, SourceHash: req.SourceHash, Timestamp: time.Now().UTC()}}, nil
}

func machineDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") || len(s) != 71 {
		return false
	}
	return strings.Trim(s[7:], "0123456789abcdef") == ""
}

func validateMachineExport(root string, base *images.MacOSImage) error {
	directory, err := os.Lstat(root)
	if err != nil || !directory.IsDir() {
		return fmt.Errorf("export root must be a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 3 {
		return fmt.Errorf("export must contain only machine payload files")
	}
	for _, entry := range entries {
		if entry.Name() != "disk.img" && entry.Name() != "aux.img" && entry.Name() != "config.json" {
			return fmt.Errorf("unexpected export file")
		}
		info, err := os.Lstat(filepath.Join(root, entry.Name()))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("export files must be regular")
		}
	}
	platform, err := images.ValidateMacOSBundle(root)
	if err != nil {
		return err
	}
	if !bytes.Equal(platform.HardwareModel, base.HardwareModel) || !bytes.Equal(platform.MachineIdentifier, base.MachineIdentifier) || !strings.EqualFold(platform.MAC, base.MAC) || platform.CPUs != base.CPUs || platform.Memory != base.Memory {
		return fmt.Errorf("machine export changed preserved identity/resources")
	}
	for _, entry := range entries {
		if err = os.Chmod(filepath.Join(root, entry.Name()), 0600); err != nil {
			return err
		}
	}
	return nil
}
