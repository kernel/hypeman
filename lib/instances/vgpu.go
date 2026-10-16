package instances

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sort"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

func (m *manager) createVGPUDevice(ctx context.Context, profileName, instanceID string) (*devices.VGPUDevice, error) {
	create := m.createVGPU
	if create == nil {
		create = devices.CreateVGPU
	}
	return create(ctx, profileName, instanceID)
}

func (m *manager) discoverVGPUDevices() (devices.VGPUFramework, []devices.VirtualFunction, error) {
	discover := m.discoverVGPU
	if discover == nil {
		discover = devices.DiscoverVGPU
	}
	return discover()
}

func (m *manager) claimVGPU(ctx context.Context, meta *metadata, profileName string) (*devices.VGPUDevice, error) {
	framework, _, err := m.discoverVGPUDevices()
	if err != nil {
		return nil, err
	}
	if framework != devices.VGPUFrameworkVendorVFIO {
		return m.createVGPUDevice(ctx, profileName, meta.Id)
	}

	m.vgpuAllocationMu.Lock()
	defer m.vgpuAllocationMu.Unlock()

	allMetadata, err := m.listMetadataForReconcile()
	if err != nil {
		return nil, fmt.Errorf("list instances for vGPU allocation: %w", err)
	}
	// VF state is read under the lock so Allocated reflects resets made by
	// the claim that held it before us.
	_, vfs, err := m.discoverVGPUDevices()
	if err != nil {
		return nil, err
	}
	listProfiles := m.vendorVFIOProfiles
	if listProfiles == nil {
		listProfiles = devices.ListVendorVFIOProfileTypes
	}
	profilesByVF, err := listProfiles(vfs)
	if err != nil {
		return nil, err
	}
	m.rememberVGPUProfiles(vfs, profilesByVF)
	// Quarantine is read under the allocation lock but mutated under the
	// devices lock, so this is a snapshot. configure re-checks it under the
	// devices lock before the VF is touched.
	quarantined, err := m.quarantinedVFAddresses()
	if err != nil {
		return nil, err
	}
	candidates := vendorVFIOCandidates{vfs: vfs, profilesByVF: profilesByVF, supported: m.supportedVGPUProfiles, claims: allMetadata, quarantined: quarantined}
	// refreshCandidates re-reads VF state and creatable lists after dirty VFs
	// were reset, keeping only the VFs still in candidates.
	refreshCandidates := func() error {
		_, vfs, err := m.discoverVGPUDevices()
		if err != nil {
			return err
		}
		vfs = slices.DeleteFunc(vfs, func(vf devices.VirtualFunction) bool {
			_, ok := vfByAddress(candidates.vfs, vf.PCIAddress)
			return !ok
		})
		profilesByVF, err := listProfiles(vfs)
		if err != nil {
			return err
		}
		m.rememberVGPUProfiles(vfs, profilesByVF)
		candidates.vfs, candidates.profilesByVF = vfs, profilesByVF
		return nil
	}
	vfAddress, profileType, err := selectVendorVFIOVF(candidates, profileName, m.pickVFIndex)
	dirtyReset := err != nil
	if err != nil {
		// A dirty unclaimed VF consumes framebuffer, which can make the
		// requested profile vanish from every creatable list before the
		// selector's repair path can resolve it. Reset such VFs and retry.
		if !m.resetDirtyUnclaimedVFs(ctx, vfs, allMetadata) {
			return nil, err
		}
		if err = refreshCandidates(); err != nil {
			return nil, err
		}
		if vfAddress, profileType, err = selectVendorVFIOVF(candidates, profileName, m.pickVFIndex); err != nil {
			return nil, err
		}
	}
	// A dirty VF carries the previous assignment's device instance and
	// possibly its open VFIO handles, even when the leftover type matches
	// the requested profile. Reset it (in-use gated inside destroy) before
	// claiming; the allocation lock keeps it unclaimed until then. A VF that
	// refuses its reset is dropped from the candidates so it cannot block
	// clean siblings.
	//
	// The VF is then configured here, under the allocation lock, so a claim
	// cannot lose its placement to a later claim once the lock is released.
	// Creatable lists are a snapshot and hide some profiles that still fit,
	// so only the driver can say whether a placement remains; the next GPU is
	// tried if it rejects the type. Every reselection passes through the
	// dirty-VF reset again.
	log := logger.FromContext(ctx)
	for {
		vf, ok := vfByAddress(candidates.vfs, vfAddress)
		if !ok {
			return nil, fmt.Errorf("selected VF %s is not a candidate", vfAddress)
		}
		if vf.Allocated {
			assignment := devices.VGPUAssignment{
				Framework:  devices.VGPUFrameworkVendorVFIO,
				DevicePath: filepath.Clean(devices.GetDeviceSysfsPath(vf.PCIAddress)),
			}
			if repairErr := m.destroyVGPUAssignment(ctx, assignment); repairErr != nil {
				log.WarnContext(ctx, "dirty vGPU VF refused reset; trying another VF", "vf", vf.PCIAddress, "error", repairErr)
				candidates.vfs = withoutVF(candidates.vfs, vf.PCIAddress)
				if vfAddress, profileType, err = selectVendorVFIOVF(candidates, profileName, m.pickVFIndex); err != nil {
					return nil, fmt.Errorf("repair dirty VF %s before claim: %w", vf.PCIAddress, repairErr)
				}
				continue
			}
			log.InfoContext(ctx, "reset dirty vGPU VF before claim", "vf", vf.PCIAddress)
		}
		placeErr := m.placeVGPU(ctx, candidates.vfs, allMetadata, vf, profileType)
		if placeErr == nil {
			if !advertisingGPUs(candidates, profileType)[vf.ParentGPU] {
				log.InfoContext(ctx, "placed vGPU profile its GPU no longer advertises", "vf", vfAddress, "profile", profileName)
			}
			break
		}
		if !errors.Is(placeErr, devices.ErrVGPUTypeRejected) {
			return nil, placeErr
		}
		log.InfoContext(ctx, "driver refused vGPU profile on GPU; trying another GPU", "gpu", vf.ParentGPU, "vf", vfAddress, "profile", profileName, "error", placeErr)
		m.recordVGPUDriverRejection(ctx, vf.ParentGPU)
		candidates.vfs = withoutGPU(candidates.vfs, vf.ParentGPU)
		vfAddress, profileType, err = selectVendorVFIOVF(candidates, profileName, m.pickVFIndex)
		// A hidden candidate can satisfy the first selection while dirty VFs
		// fill a GPU that has no candidate, so the dirty-VF retry above did
		// not run. Run it once before giving up.
		if err != nil && !dirtyReset && m.resetDirtyUnclaimedVFs(ctx, candidates.vfs, allMetadata) {
			dirtyReset = true
			if err = refreshCandidates(); err != nil {
				return nil, err
			}
			vfAddress, profileType, err = selectVendorVFIOVF(candidates, profileName, m.pickVFIndex)
		}
		if err != nil {
			return nil, fmt.Errorf("%w (driver refused it on GPU %s: %v)", err, vf.ParentGPU, placeErr)
		}
	}
	device := &devices.VGPUDevice{
		Framework:   devices.VGPUFrameworkVendorVFIO,
		VFAddress:   vfAddress,
		ProfileType: profileType,
		ProfileName: profileName,
		SysfsPath:   filepath.Clean(devices.GetDeviceSysfsPath(vfAddress)),
	}
	setStoredVGPUDevice(&meta.StoredMetadata, device)
	// Only vendor VFIO claims carry an assignment identity, so the claim time
	// is set here rather than in setStoredVGPUDevice. clearStoredVGPUDevice
	// resets it with the rest of the device fields.
	claimedAt := m.nowUTC()
	meta.GPUClaimedAt = &claimedAt
	if err := m.saveMetadata(meta); err != nil {
		clearStoredVGPUDevice(&meta.StoredMetadata)
		if resetErr := m.destroyVGPUAssignment(ctx, devices.VGPUAssignment{Framework: device.Framework, DevicePath: device.SysfsPath}); resetErr != nil {
			log.WarnContext(ctx, "failed to reset vGPU VF after claim save failed", "vf", vfAddress, "error", resetErr)
		}
		return nil, fmt.Errorf("save vGPU claim: %w", err)
	}
	return device, nil
}

// resetDirtyUnclaimedVFs resets allocated VFs that no instance claims,
// restoring the framebuffer their leftover type consumes. The reset is
// gated on the open-VFIO-handle in-use check inside destroy. Reports
// whether any VF was reset. Callers must hold vgpuAllocationMu.
func (m *manager) resetDirtyUnclaimedVFs(ctx context.Context, vfs []devices.VirtualFunction, allMetadata []StoredMetadata) bool {
	claimed := make(map[string]struct{}, len(allMetadata))
	for i := range allMetadata {
		if path := allMetadata[i].GPUDevicePath; path != "" {
			claimed[filepath.Base(path)] = struct{}{}
		}
	}
	log := logger.FromContext(ctx)
	reset := false
	for _, vf := range vfs {
		if !vf.Allocated {
			continue
		}
		if _, ok := claimed[vf.PCIAddress]; ok {
			continue
		}
		assignment := devices.VGPUAssignment{
			Framework:  devices.VGPUFrameworkVendorVFIO,
			DevicePath: filepath.Clean(devices.GetDeviceSysfsPath(vf.PCIAddress)),
		}
		if err := m.destroyVGPUAssignment(ctx, assignment); err != nil {
			log.WarnContext(ctx, "failed to reset dirty vGPU VF", "vf", vf.PCIAddress, "error", err)
			continue
		}
		log.InfoContext(ctx, "reset dirty vGPU VF during allocation", "vf", vf.PCIAddress)
		reset = true
	}
	return reset
}

// placeVGPU configures vf with profileType, so the driver decides whether a
// placement remains. An unclaimed dirty sibling can hold the last placement,
// so when the driver rejects the type, those siblings are reset (in-use
// gated) and the write is retried once. A VF left configured by a crash before
// its claim was saved is such a sibling for the next allocation. Callers must
// hold vgpuAllocationMu.
func (m *manager) placeVGPU(ctx context.Context, vfs []devices.VirtualFunction, allMetadata []StoredMetadata, vf devices.VirtualFunction, profileType string) error {
	device := &devices.VGPUDevice{
		Framework:   devices.VGPUFrameworkVendorVFIO,
		VFAddress:   vf.PCIAddress,
		ProfileType: profileType,
	}
	err := m.configureClaimedVGPU(ctx, device)
	if errors.Is(err, devices.ErrVGPUTypeRejected) && m.resetDirtyUnclaimedVFs(ctx, siblingVFs(vfs, vf), allMetadata) {
		err = m.configureClaimedVGPU(ctx, device)
	}
	return err
}

// rememberVGPUProfiles records every profile each GPU's VFs advertise. In
// mixed-size mode the driver drops a profile from every creatable list on a
// GPU once that GPU holds as many vGPUs as the profile's equal-size instance
// limit (12 for L40S-4Q), even while a placement for it remains, so the
// current lists alone under-report what a busy GPU can still take. Callers
// must hold vgpuAllocationMu.
func (m *manager) rememberVGPUProfiles(vfs []devices.VirtualFunction, profilesByVF map[string][]devices.VGPUProfileType) {
	if m.supportedVGPUProfiles == nil {
		m.supportedVGPUProfiles = make(map[string]map[string]devices.VGPUProfileType)
	}
	for _, vf := range vfs {
		for _, profile := range profilesByVF[vf.PCIAddress] {
			byName := m.supportedVGPUProfiles[vf.ParentGPU]
			if byName == nil {
				byName = make(map[string]devices.VGPUProfileType)
				m.supportedVGPUProfiles[vf.ParentGPU] = byName
			}
			byName[profile.Name] = profile
		}
	}
}

func (m *manager) quarantinedVFAddresses() (map[string]struct{}, error) {
	quarantined := m.quarantinedVFs
	if quarantined == nil {
		quarantined = devices.QuarantinedVFAddresses
	}
	return quarantined()
}

// vendorVFIOCandidates is the host state a vendor VFIO placement chooses from.
type vendorVFIOCandidates struct {
	vfs          []devices.VirtualFunction
	profilesByVF map[string][]devices.VGPUProfileType
	supported    map[string]map[string]devices.VGPUProfileType // parent GPU -> profile name -> profile, as remembered by rememberVGPUProfiles
	claims       []StoredMetadata                              // every instance; those with a device path hold a VF
	quarantined  map[string]struct{}
}

// selectVendorVFIOVF picks the VF to claim for profileName. Quarantined VFs
// are never candidates and count against their parent GPU, so placement
// drifts away from cards carrying a wedged VF. Among equally ranked
// candidates on the chosen GPU, pick selects the index (nil is uniform
// random), so a single VF cannot capture every placement on an idle host.
//
// A GPU that supports the profile but no longer advertises it is still a
// candidate for a clean VF, ranked after every GPU that does advertise it,
// because the driver hides profiles by instance count rather than by free
// placement. The caller must confirm such a choice with the driver. A free
// VF that advertises nothing is not a candidate for this: on L40S an empty
// list means its GPU has no placement left for any size, and a VF whose list
// could not be read is unknown.
func selectVendorVFIOVF(c vendorVFIOCandidates, profileName string, pick func(n int) int) (string, string, error) {
	profilesByName := make(map[string]devices.VGPUProfileType)
	for _, byName := range c.supported {
		for name, profile := range byName {
			profilesByName[name] = profile
		}
	}
	advertises := make(map[string]map[string]struct{}, len(c.profilesByVF))
	for vfAddress, profiles := range c.profilesByVF {
		advertises[vfAddress] = make(map[string]struct{}, len(profiles))
		for _, profile := range profiles {
			profilesByName[profile.Name] = profile
			advertises[vfAddress][profile.TypeName] = struct{}{}
		}
	}
	// Only VFs still in c.vfs count as advertising, so a profile whose
	// advertising GPUs were all dropped after a driver rejection reports as
	// not creatable.
	advertisedNames := make(map[string]struct{})
	for _, vf := range c.vfs {
		for _, profile := range c.profilesByVF[vf.PCIAddress] {
			advertisedNames[profile.Name] = struct{}{}
		}
	}
	notCreatable := func() error {
		if len(advertisedNames) == 0 && len(c.vfs) > 0 {
			return fmt.Errorf("no creatable vGPU profiles on any VF (GPUs at capacity or dirty VFs consuming framebuffer): profile %q", profileName)
		}
		return fmt.Errorf("profile %q is not creatable on any VF (unknown profile or insufficient capacity)", profileName)
	}
	requested, found := profilesByName[profileName]
	if !found {
		return "", "", notCreatable()
	}

	vfsByAddress := make(map[string]devices.VirtualFunction, len(c.vfs))
	for _, vf := range c.vfs {
		vfsByAddress[vf.PCIAddress] = vf
	}
	claimed := make(map[string]struct{})
	usageByGPU := make(map[string]int)
	unknownUsageByGPU := make(map[string]bool)
	for i := range c.claims {
		stored := &c.claims[i]
		if stored.GPUDevicePath == "" {
			continue
		}
		vfAddress := filepath.Base(stored.GPUDevicePath)
		claimed[vfAddress] = struct{}{}
		vf, ok := vfsByAddress[vfAddress]
		if !ok {
			continue
		}
		profile, ok := profilesByName[stored.GPUProfile]
		if !ok {
			unknownUsageByGPU[vf.ParentGPU] = true
			continue
		}
		usageByGPU[vf.ParentGPU] += profile.FramebufferMB
	}

	parentAdvertises := advertisingGPUs(c, requested.TypeName)
	quarantinedByGPU := make(map[string]int)
	freeByGPU := make(map[string][]devices.VirtualFunction)
	for _, vf := range c.vfs {
		if _, bad := c.quarantined[vf.PCIAddress]; bad {
			quarantinedByGPU[vf.ParentGPU]++
			continue
		}
		if _, ok := claimed[vf.PCIAddress]; ok {
			continue
		}
		_, directlyCreatable := advertises[vf.PCIAddress][requested.TypeName]
		repairable := vf.Allocated && (vf.ProfileType == requested.TypeName || parentAdvertises[vf.ParentGPU])
		supported, ok := c.supported[vf.ParentGPU][profileName]
		hidden := !vf.Allocated && !parentAdvertises[vf.ParentGPU] && len(advertises[vf.PCIAddress]) > 0 &&
			ok && supported.TypeName == requested.TypeName
		if directlyCreatable || repairable || hidden {
			freeByGPU[vf.ParentGPU] = append(freeByGPU[vf.ParentGPU], vf)
		}
	}
	// Clean VFs first: a dirty VF needs a hardware reset before it can be
	// claimed, and that reset can be refused while a stale handle is open.
	for gpu := range freeByGPU {
		sort.Slice(freeByGPU[gpu], func(i, j int) bool {
			a, b := freeByGPU[gpu][i], freeByGPU[gpu][j]
			if a.Allocated != b.Allocated {
				return !a.Allocated
			}
			return a.PCIAddress < b.PCIAddress
		})
	}
	gpus := make([]string, 0, len(freeByGPU))
	for gpu := range freeByGPU {
		gpus = append(gpus, gpu)
	}
	sort.Slice(gpus, func(i, j int) bool {
		if parentAdvertises[gpus[i]] != parentAdvertises[gpus[j]] {
			return parentAdvertises[gpus[i]]
		}
		if quarantinedByGPU[gpus[i]] != quarantinedByGPU[gpus[j]] {
			return quarantinedByGPU[gpus[i]] < quarantinedByGPU[gpus[j]]
		}
		if unknownUsageByGPU[gpus[i]] != unknownUsageByGPU[gpus[j]] {
			return !unknownUsageByGPU[gpus[i]]
		}
		if usageByGPU[gpus[i]] == usageByGPU[gpus[j]] {
			return gpus[i] < gpus[j]
		}
		return usageByGPU[gpus[i]] < usageByGPU[gpus[j]]
	})
	if len(gpus) == 0 {
		if _, ok := advertisedNames[profileName]; !ok {
			return "", "", notCreatable()
		}
		return "", "", fmt.Errorf("no available VF for profile %q", profileName)
	}
	// Candidates are sorted clean-first, so the leading run with the same
	// Allocated state is the set of equally ranked VFs.
	candidates := freeByGPU[gpus[0]]
	n := 1
	for n < len(candidates) && candidates[n].Allocated == candidates[0].Allocated {
		n++
	}
	if pick == nil {
		pick = rand.IntN
	}
	return candidates[pick(n)].PCIAddress, requested.TypeName, nil
}

func advertisingGPUs(c vendorVFIOCandidates, profileType string) map[string]bool {
	gpus := make(map[string]bool)
	for _, vf := range c.vfs {
		for _, profile := range c.profilesByVF[vf.PCIAddress] {
			if profile.TypeName == profileType {
				gpus[vf.ParentGPU] = true
				break
			}
		}
	}
	return gpus
}

func (m *manager) configureClaimedVGPU(ctx context.Context, device *devices.VGPUDevice) error {
	if device.Framework != devices.VGPUFrameworkVendorVFIO {
		return nil
	}
	configure := m.configureVGPU
	if configure == nil {
		configure = devices.ConfigureVGPU
	}
	return configure(ctx, device.VFAddress, device.ProfileType)
}

func (m *manager) destroyVGPUAssignment(ctx context.Context, assignment devices.VGPUAssignment) error {
	destroy := m.destroyVGPU
	if destroy == nil {
		destroy = devices.DestroyVGPU
	}
	return destroy(ctx, assignment)
}

func setStoredVGPUDevice(stored *StoredMetadata, device *devices.VGPUDevice) {
	stored.GPUFramework = device.Framework
	stored.GPUDevicePath = device.SysfsPath
	stored.GPUMdevUUID = device.MdevUUID
}

func clearStoredVGPUDevice(stored *StoredMetadata) {
	stored.GPUFramework = devices.VGPUFrameworkNone
	stored.GPUDevicePath = ""
	stored.GPUMdevUUID = ""
	stored.GPUClaimedAt = nil
}

// vgpuHypervisorMayBeAlive treats ambiguous ownership as live, so it never
// authorizes a release, and records why ownership could not be established
// so retained capacity remains diagnosable.
func (m *manager) vgpuHypervisorMayBeAlive(ctx context.Context, stored *StoredMetadata) bool {
	pid, err := resolveLiveHypervisorPID(stored.HypervisorProcessIdentity, stored.SocketPath)
	if err == nil {
		return pid > 0
	}
	logger.FromContext(ctx).WarnContext(ctx, "preserving vGPU claim because hypervisor liveness is uncertain",
		"instance_id", stored.Id,
		"device_path", storedVGPUDevicePath(stored),
		"error", err)
	m.recordVGPULivenessUncertain(ctx)
	return true
}

// vgpuCleanupGuard checks whether the VF can be safely released: the VMM
// must be dead and, for vendor VFIO, the on-disk claim must still match.
// Returns the device path, or "" when cleanup must be skipped.
func (m *manager) vgpuCleanupGuard(ctx context.Context, stored *StoredMetadata) string {
	path := storedVGPUDevicePath(stored)
	if path == "" {
		return ""
	}
	if m.vgpuHypervisorMayBeAlive(ctx, stored) {
		return ""
	}
	if stored.GPUFramework == devices.VGPUFrameworkVendorVFIO {
		onDisk, err := m.loadMetadata(stored.Id)
		if err != nil || onDisk.GPUDevicePath != stored.GPUDevicePath {
			logger.FromContext(ctx).WarnContext(ctx, "skipping vGPU cleanup without matching claim", "instance_id", stored.Id, "device_path", path, "error", err)
			return ""
		}
	}
	return path
}

func (m *manager) cleanupCreateVGPU(ctx context.Context, stored *StoredMetadata) bool {
	path := m.vgpuCleanupGuard(ctx, stored)
	if path == "" && storedVGPUDevicePath(stored) != "" {
		return false
	}
	if path == "" {
		return true
	}
	if err := m.destroyVGPUAssignment(ctx, vgpuAssignment(stored)); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to reset vGPU during create cleanup", "instance_id", stored.Id, "device_path", path, "error", err)
	}
	return true
}

// cleanupStartVGPU rolls back a vGPU claim after a failed start. The caller
// holds the instance lock; this function acquires vgpuAllocationMu, matching
// the instance-lock-before-allocation-lock ordering used by claimVGPU.
func (m *manager) cleanupStartVGPU(ctx context.Context, current *StoredMetadata, rollback metadata) {
	path := m.vgpuCleanupGuard(ctx, current)
	if path == "" {
		return
	}
	if err := m.destroyVGPUAssignment(ctx, vgpuAssignment(current)); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to reset vGPU during start cleanup", "instance_id", current.Id, "device_path", path, "error", err)
	}
	m.vgpuAllocationMu.Lock()
	defer m.vgpuAllocationMu.Unlock()
	onDisk, err := m.loadMetadata(current.Id)
	if err != nil || storedVGPUDevicePath(&onDisk.StoredMetadata) != path {
		return
	}
	if err := m.saveMetadata(&rollback); err != nil {
		logger.FromContext(ctx).ErrorContext(ctx, "failed to restore metadata after vGPU cleanup", "instance_id", current.Id, "error", err)
	}
}

func (m *manager) releaseStoredVGPU(ctx context.Context, stored *StoredMetadata) error {
	if storedVGPUDevicePath(stored) != "" {
		if err := m.destroyVGPUAssignment(ctx, vgpuAssignment(stored)); err != nil {
			return err
		}
	}
	clearStoredVGPUDevice(stored)
	return nil
}

func (m *manager) releaseStoredVGPUPersisted(ctx context.Context, meta *metadata) error {
	if err := m.releaseStoredVGPU(ctx, &meta.StoredMetadata); err != nil {
		return err
	}
	m.vgpuAllocationMu.Lock()
	defer m.vgpuAllocationMu.Unlock()
	return m.saveMetadata(meta)
}

func vgpuAssignment(stored *StoredMetadata) devices.VGPUAssignment {
	return devices.VGPUAssignment{
		Framework:  stored.GPUFramework,
		DevicePath: storedVGPUDevicePath(stored),
		MdevUUID:   stored.GPUMdevUUID,
		InstanceID: stored.Id,
	}
}

func vfByAddress(vfs []devices.VirtualFunction, address string) (devices.VirtualFunction, bool) {
	for _, vf := range vfs {
		if vf.PCIAddress == address {
			return vf, true
		}
	}
	return devices.VirtualFunction{}, false
}

func withoutVF(vfs []devices.VirtualFunction, address string) []devices.VirtualFunction {
	remaining := make([]devices.VirtualFunction, 0, len(vfs))
	for _, vf := range vfs {
		if vf.PCIAddress != address {
			remaining = append(remaining, vf)
		}
	}
	return remaining
}

func siblingVFs(vfs []devices.VirtualFunction, vf devices.VirtualFunction) []devices.VirtualFunction {
	siblings := make([]devices.VirtualFunction, 0, len(vfs))
	for _, other := range vfs {
		if other.ParentGPU == vf.ParentGPU && other.PCIAddress != vf.PCIAddress {
			siblings = append(siblings, other)
		}
	}
	return siblings
}

func withoutGPU(vfs []devices.VirtualFunction, parentGPU string) []devices.VirtualFunction {
	remaining := make([]devices.VirtualFunction, 0, len(vfs))
	for _, vf := range vfs {
		if vf.ParentGPU != parentGPU {
			remaining = append(remaining, vf)
		}
	}
	return remaining
}

func storedVGPUDevicePath(stored *StoredMetadata) string {
	if stored.GPUDevicePath != "" {
		return stored.GPUDevicePath
	}
	if stored.GPUMdevUUID != "" {
		return filepath.Join("/sys/bus/mdev/devices", stored.GPUMdevUUID)
	}
	return ""
}
