package instances

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/network"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
)

// macOSAuxStorageFile is the instance's copy of the image's auxiliary storage.
const macOSAuxStorageFile = "mac-aux.img"

// requireMacOSImageName refuses a create on a macOS-only server unless the image
// is an already imported macOS image, so a Linux image is never pulled there.
func (m *manager) requireMacOSImageName(ctx context.Context, name string) error {
	if !m.macOSOnly {
		return nil
	}
	cached, err := m.imageManager.GetImage(ctx, name)
	if err != nil && !errors.Is(err, images.ErrNotFound) {
		return err
	}
	if err != nil || cached.MacOS == nil {
		return fmt.Errorf("%w: macOS-only server requires an already imported macOS image", ErrInvalidRequest)
	}
	return nil
}

// checkMacOSCreate applies macOS machine defaults to a create from a macOS image.
// Linux images pass through unchanged, except on a macOS-only server.
func (m *manager) checkMacOSCreate(req *CreateInstanceRequest, img *images.Image, hvType hypervisor.Type) error {
	if img.MacOS == nil {
		if m.macOSOnly {
			return fmt.Errorf("%w: server is configured for macOS guests only", ErrInvalidRequest)
		}
		return nil
	}
	caps, _ := hypervisor.CapabilitiesForType(hvType)
	prepared := *req
	if err := prepareMacOSCreate(&prepared, img, caps); err != nil {
		return err
	}
	starter, err := m.getVMStarter(hvType)
	if err != nil {
		return err
	}
	config := hypervisor.VMConfig{
		BootMode: hypervisor.BootModeMacOS, VCPUs: prepared.Vcpus, MemoryBytes: prepared.Size,
		MacOS: &hypervisor.MacOSPlatform{HardwareModelData: img.MacOS.HardwareModel, MachineIdentifierData: img.MacOS.MachineIdentifier, AuxStoragePath: "mac-aux.img"},
		Disks: []hypervisor.DiskConfig{{Path: "disk.img"}},
	}
	if err := validatePlannedVMConfig(starter, hvType, config); err != nil {
		return err
	}
	*req = prepared
	return nil
}

// prepareMacOSCreate validates explicit options before applying machine defaults.
// Rejected requests are unchanged; effective CPU and memory are resolved once.
func prepareMacOSCreate(req *CreateInstanceRequest, img *images.Image, caps hypervisor.Capabilities) error {
	if img.MacOS == nil {
		return fmt.Errorf("%w: image is not a macOS disk bundle", ErrInvalidRequest)
	}
	if !caps.SupportsMacOSBoot {
		return fmt.Errorf("%w: macOS guests require vz on Apple silicon", ErrInvalidRequest)
	}
	if img.Platform != "darwin/arm64" || img.SizeBytes == nil || *img.SizeBytes <= 0 {
		return fmt.Errorf("%w: incomplete macOS image", ErrImageNotReady)
	}
	if req.HotplugSize != 0 || req.OverlaySize != 0 || len(req.Volumes) != 0 || len(req.Devices) != 0 || req.GPU != nil || len(req.Env) != 0 || len(req.Entrypoint) != 0 || len(req.Cmd) != 0 || req.NetworkEgress != nil || len(req.Credentials) != 0 || req.AutoStandby != nil || req.SnapshotPolicy != nil || req.DiskIOBps != 0 || req.NetworkBandwidthDownload != 0 || req.NetworkBandwidthUpload != 0 {
		return fmt.Errorf("%w: experimental macOS supports local disk clone, CPU/RAM, tags, expiration and NAT only; Linux commands/env/volumes, overlays, auto-standby/snapshot policies and I/O shaping are unsupported", ErrInvalidRequest)
	}
	if err := validateMacOSRestartPolicy(req.RestartPolicy); err != nil {
		return err
	}
	if err := validateMacOSHealthCheck(req.HealthCheck, img.MacOS.GuestAgent && !req.SkipGuestAgent); err != nil {
		return err
	}
	size, vcpus := req.Size, req.Vcpus
	if size == 0 {
		size = int64(img.MacOS.Memory)
	}
	if vcpus == 0 {
		vcpus = int(img.MacOS.CPUs)
	}
	if size < 4<<30 || vcpus < 2 || size%(1<<20) != 0 {
		return fmt.Errorf("%w: macOS requires at least 4 GiB in whole MiB and 2 vCPUs", ErrInvalidRequest)
	}
	req.Size, req.Vcpus = size, vcpus
	req.OverlaySize = *img.SizeBytes // Reserve the writable boot disk, not a Linux overlay.
	req.SkipGuestAgent = req.SkipGuestAgent || !img.MacOS.GuestAgent
	req.SkipKernelHeaders = true
	return nil
}

func macOSPlatformForCreate(platform *images.MacOSImage, size int64, vcpus int) *images.MacOSImage {
	if platform == nil {
		return nil
	}
	configured := *platform
	configured.Memory, configured.CPUs = uint64(size), uint(vcpus)
	return &configured
}

func validateMacOSRestartPolicy(policy *restartpolicy.Policy) error {
	if policy != nil && policy.Policy == restartpolicy.PolicyOnFailure {
		return fmt.Errorf("%w: macOS supports restart_policy always or never; workload exit status is unavailable", ErrInvalidRequest)
	}
	return nil
}

func checkMacOSUpdate(stored *StoredMetadata, req UpdateInstanceRequest) error {
	if stored.MacOS == nil {
		return nil
	}
	if len(req.Env) != 0 || req.AutoStandby != nil {
		return fmt.Errorf("%w: macOS updates support expiration, exec health checks and VM restart policy only", ErrInvalidRequest)
	}
	if err := validateMacOSHealthCheck(req.HealthCheck, stored.GuestAgentEnabled()); err != nil {
		return err
	}
	return validateMacOSRestartPolicy(req.RestartPolicy)
}

func validateMacOSHealthCheck(policy *healthcheck.Policy, agentEnabled bool) error {
	normalized, err := normalizeHealthCheckPolicy(policy)
	if err != nil {
		return err
	}
	if !healthcheck.Enabled(normalized) {
		return nil
	}
	if normalized.Type != healthcheck.TypeExec || !agentEnabled {
		return fmt.Errorf("%w: macOS supports exec health checks with an enabled guest agent only", ErrInvalidRequest)
	}
	return nil
}

// checkMacOSStart refuses starts a macOS-only server or a macOS guest cannot honor.
func (m *manager) checkMacOSStart(stored *StoredMetadata, req StartInstanceRequest) error {
	if err := m.requireMacOSGuest(stored); err != nil {
		return err
	}
	if stored.MacOS != nil && (len(req.Entrypoint) != 0 || len(req.Cmd) != 0) {
		return fmt.Errorf("%w: macOS start does not support command overrides", ErrInvalidRequest)
	}
	return nil
}

// createBootDisk creates the Linux overlay, or clones a macOS image's boot disk and
// aux storage into the instance.
func (m *manager) createBootDisk(stored *StoredMetadata, img *images.Image) error {
	if stored.MacOS == nil {
		return m.createOverlayDisk(stored.Id, stored.OverlaySize)
	}
	disk, err := images.GetDiskPath(m.paths, img.Name, img.Digest)
	if err != nil {
		return err
	}
	aux := filepath.Join(filepath.Dir(disk), "aux.img")
	return cloneMacOSStorage(disk, aux, m.paths.InstanceOverlay(stored.Id), filepath.Join(stored.DataDir, macOSAuxStorageFile))
}

// applyMacOSNetwork sets the template MAC on stored. vmnet assigns the address, which is
// observed after boot, so no Linux TAP or static IP is allocated.
func applyMacOSNetwork(stored *StoredMetadata) *network.NetworkConfig {
	stored.MAC = stored.MacOS.MAC
	stored.IP = ""
	return &network.NetworkConfig{MAC: stored.MAC}
}

func (m *manager) macOSVMConfig(inst *Instance) hypervisor.VMConfig {
	c := hypervisor.VMConfig{BootMode: hypervisor.BootModeMacOS, VCPUs: inst.Vcpus, MemoryBytes: inst.Size, VsockCID: inst.VsockCID, VsockSocket: inst.VsockSocket,
		MacOS: &hypervisor.MacOSPlatform{HardwareModelData: inst.MacOS.HardwareModel, MachineIdentifierData: inst.MacOS.MachineIdentifier, AuxStoragePath: filepath.Join(inst.DataDir, macOSAuxStorageFile)},
		Disks: []hypervisor.DiskConfig{{Path: m.paths.InstanceOverlay(inst.Id)}}}
	if inst.NetworkEnabled {
		c.Networks = []hypervisor.NetworkConfig{{MAC: inst.MAC}}
	}
	return c
}
func initialBootPhase(stored *StoredMetadata) phasetracking.Phase {
	if stored.MacOS != nil {
		phase, _ := macOSRunningPhase(stored)
		return phase
	}
	return phasetracking.PhaseInitializing
}
func (m *manager) checkMacOSIdentityAvailable(ctx context.Context, stored *StoredMetadata) error {
	// A shim left by an interrupted boot or restore may still be loading the VM
	// before it opens its socket. Booting another would run two VMs on one disk.
	pids, err := hypervisor.VZShimPIDs(stored.SocketPath)
	if err != nil {
		return fmt.Errorf("check for a running vz-shim: %w", err)
	}
	if len(pids) > 0 {
		return fmt.Errorf("%w: a vz-shim (pid %d) is already running for this instance; if an interrupted operation left it, wait for it or delete the instance", ErrInvalidState, pids[0])
	}
	return m.forEachOtherInstance(stored.Id, "check Mac identity admission", func(meta *metadata) error {
		if meta.MacOS == nil || !bytes.Equal(meta.MacOS.MachineIdentifier, stored.MacOS.MachineIdentifier) {
			return nil
		}
		state := m.deriveStateWithoutHydration(ctx, &meta.StoredMetadata).State
		if state.RequiresVMM() || state == StateUnknown {
			return fmt.Errorf("%w: macOS template identity is already in use by instance %s; stop it first", ErrInsufficientResources, meta.Id)
		}
		return nil
	})
}

// GuestAgentEnabled reports whether exec, copy, readiness and shutdown may use the
// shared guest agent. A macOS image must declare it; hand-written metadata that
// leaves SkipGuestAgent unset does not enable it.
func (s *StoredMetadata) GuestAgentEnabled() bool {
	return !s.SkipGuestAgent && (s.MacOS == nil || s.MacOS.GuestAgent)
}

// programMarkerSettled reports that no workload-start marker is awaited. macOS
// guests have no Linux workload marker, so only agent readiness is probed.
func programMarkerSettled(stored *StoredMetadata) bool {
	return stored.MacOS != nil || stored.ProgramStartedAt != nil
}

// guestAgentReadyExecutable is the readiness probe command. Darwin provides true
// in /usr/bin, not /bin.
func guestAgentReadyExecutable(stored *StoredMetadata) string {
	if stored.MacOS != nil {
		return "/usr/bin/true"
	}
	return "/bin/true"
}

// macOSMinStopTimeout is the shortest graceful stop window, in seconds, given to a
// macOS guest, so a normal stop can record a graceful receipt and strict export waits.
const macOSMinStopTimeout = 30

// hypervisorProcessLive reports whether the instance's VMM process is still running.
func hypervisorProcessLive(inst *Instance) bool {
	pid, err := resolveLiveHypervisorPID(inst.HypervisorProcessIdentity, inst.SocketPath)
	return err == nil && pid != 0
}

// admitGuestOperation applies the server's guest-type restriction. Operation-specific
// admission follows at the caller, under the instance lock.
func (m *manager) admitGuestOperation(stored *StoredMetadata, _ string) error {
	return m.requireMacOSGuest(stored)
}

// vmnet stores octets without leading zeroes. A lease is observed addressing,
// not a static-IP assignment or a guest-readiness signal.
func macOSGuestIP(mac string) string {
	wanted, err := net.ParseMAC(mac)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}
	return macOSLeaseIP(string(data), wanted)
}

// dhcpd appends newer leases, so the last matching block is the current one.
func macOSLeaseIP(data string, wanted net.HardwareAddr) string {
	found := ""
	for _, block := range strings.Split(data, "}") {
		var addr, ip string
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			switch k {
			case "hw_address":
				_, addr, _ = strings.Cut(v, ",")
			case "ip_address":
				ip = v
			}
		}
		var padded []string
		for _, octet := range strings.Split(addr, ":") {
			if len(octet) == 1 {
				octet = "0" + octet
			}
			padded = append(padded, octet)
		}
		raw, err := hex.DecodeString(strings.Join(padded, ""))
		if err == nil && string(raw) == string(wanted) && net.ParseIP(ip) != nil {
			found = ip
		}
	}
	return found
}

// VMStoragePath is the disk a macOS VM process holds open, used to find that process.
// Linux guests return empty: their hypervisor PID is the whole guest.
func (s *StoredMetadata) VMStoragePath() string {
	if s.MacOS == nil || s.DataDir == "" {
		return ""
	}
	return filepath.Join(s.DataDir, "overlay.raw")
}

// requireMacOSGuest refuses Linux guests on a macOS-only server.
func (m *manager) requireMacOSGuest(stored *StoredMetadata) error {
	if m.macOSOnly && stored.MacOS == nil {
		return fmt.Errorf("%w: server is configured for macOS guests only", ErrInvalidRequest)
	}
	return nil
}

// macOSShimGone reports whether a macOS guest's shim died without removing its
// control socket, as after SIGKILL. Nothing accepts on the socket and the
// recorded shim is gone, so the guest is stopped rather than Unknown.
func macOSShimGone(stored *StoredMetadata, err error) bool {
	if stored.MacOS == nil || !errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	return stored.HypervisorPID == nil || !ProcessExists(*stored.HypervisorPID)
}

// macOSStateWithoutShim matches the state of a guest with no control socket. No
// stop ran, so the guest's resources are also released for admission here.
func (m *manager) macOSStateWithoutShim(stored *StoredMetadata) stateResult {
	m.invalidateCachedHypervisorState(stored.Id)
	m.setAdmissionAllocationActive(stored, false)
	if m.hasSnapshot(stored.DataDir) {
		return stateResult{State: StateStandby}
	}
	return stateResult{State: StateStopped}
}

// vzShimOwner finds the vz-shim for socketPath when no live shim PID is recorded,
// as after the API dies mid-boot or mid-restore. More than one is ambiguous.
func vzShimOwner(socketPath string) (int, error) {
	if socketPath == "" {
		return 0, nil
	}
	pids, err := hypervisor.VZShimPIDs(socketPath)
	if err != nil {
		return 0, err
	}
	switch len(pids) {
	case 0:
		return 0, nil
	case 1:
		return pids[0], nil
	}
	return 0, fmt.Errorf("multiple vz-shim processes for socket %s: %v", socketPath, pids)
}

// macOSShimRefusing reports whether a macOS guest's control socket exists but
// nothing accepts on it, as after the shim is SIGKILLed.
func macOSShimRefusing(stored *StoredMetadata) bool {
	if stored.MacOS == nil {
		return false
	}
	conn, err := net.DialTimeout("unix", stored.SocketPath, time.Second)
	if err != nil {
		return errors.Is(err, syscall.ECONNREFUSED)
	}
	conn.Close()
	return false
}

// macOSRunningPhase matches deriveRunningState: an agent-enabled guest runs once
// the agent is ready; otherwise it runs from boot. A zero time is clamped to the
// phase start by the caller.
func macOSRunningPhase(stored *StoredMetadata) (phasetracking.Phase, time.Time) {
	if !stored.GuestAgentEnabled() {
		return phasetracking.PhaseRunning, time.Time{}
	}
	if stored.GuestAgentReadyAt == nil {
		return phasetracking.PhaseInitializing, time.Time{}
	}
	return phasetracking.PhaseRunning, *stored.GuestAgentReadyAt
}
