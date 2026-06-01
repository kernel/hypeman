package network

import (
	"context"
	"crypto/rand"
	"fmt"
	mathrand "math/rand"
	"net"
	"os"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
)

// CreateAllocation allocates IP/MAC/TAP for instance on the default network
func (m *manager) CreateAllocation(ctx context.Context, req AllocateRequest) (*NetworkConfig, error) {
	log := logger.FromContext(ctx)

	// Resolve bridge/default network before taking allocation lock so
	// self-heal retries don't block other allocation/release operations.
	networkCtx, networkSpanEnd := startNetworkStep(ctx, "network.get_default_network",
		attribute.String("operation", "get_default_network"),
		attribute.String("instance_id", req.InstanceID),
	)
	network, err := m.getOrInitDefaultNetwork(networkCtx)
	networkSpanEnd(err)
	if err != nil {
		return nil, err
	}

	// Reserve network identity under a short lock, then create host devices
	// outside it so concurrent forks do not queue behind TAP setup.
	waitCtx, waitSpanEnd := startNetworkStep(ctx, "network.create_allocation.wait_for_mutex",
		attribute.String("operation", "wait_for_mutex"),
		attribute.String("instance_id", req.InstanceID),
	)
	m.mu.Lock()
	waitSpanEnd(nil)

	lockedCtx, lockedSpanEnd := startNetworkStep(waitCtx, "network.create_allocation.locked",
		attribute.String("operation", "create_allocation_locked"),
		attribute.String("instance_id", req.InstanceID),
	)
	netConfig, err := m.reserveAllocationIdentityLocked(lockedCtx, network, req)
	lockedSpanEnd(err)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// Create the TAP synchronously because the VMM restore depends on it.
	tapCtx, tapSpanEnd := startNetworkStep(ctx, "network.create_tap",
		attribute.String("operation", "create_tap"),
		attribute.String("instance_id", req.InstanceID),
		attribute.String("tap", netConfig.TAPDevice),
		attribute.Bool("isolated", network.Isolated),
		attribute.Bool("download_rate_limit", req.DownloadBps > 0),
		attribute.Bool("upload_rate_limit", req.UploadBps > 0),
	)
	err = m.createTAPDevice(tapCtx, netConfig.TAPDevice, network.Bridge, network.Isolated)
	tapSpanEnd(err)
	if err != nil {
		m.forgetPendingAllocation(req.InstanceID)
		_ = m.deleteTAPDeviceSerialized(netConfig.TAPDevice, "")
		return nil, fmt.Errorf("create TAP device: %w", err)
	}
	m.recordTAPOperation(ctx, "create")

	if req.DownloadBps > 0 || req.UploadBps > 0 {
		m.enqueueRateLimit(ctx, rateLimitRequest{
			instanceID:    req.InstanceID,
			tapName:       netConfig.TAPDevice,
			bridgeName:    network.Bridge,
			downloadBps:   req.DownloadBps,
			uploadBps:     req.UploadBps,
			uploadCeilBps: req.UploadCeilBps,
		})
	} else {
		m.clearClassID(req.InstanceID)
	}

	log.InfoContext(ctx, "allocated network",
		"instance_id", req.InstanceID,
		"instance_name", req.InstanceName,
		"network", "default",
		"ip", netConfig.IP,
		"mac", netConfig.MAC,
		"tap", netConfig.TAPDevice,
		"download_bps", req.DownloadBps,
		"upload_bps", req.UploadBps)

	return netConfig, nil
}

// RecreateAllocation recreates TAP for restore from standby
// Note: No lock needed - this operation:
// 1. Doesn't allocate new IPs (reuses existing from snapshot)
// 2. Is already protected by instance-level locking
// 3. Uses deterministic TAP names that can't conflict
func (m *manager) RecreateAllocation(ctx context.Context, instanceID string, downloadBps, uploadBps int64) error {
	log := logger.FromContext(ctx)

	// 1. Derive allocation from snapshot
	deriveCtx, deriveSpanEnd := startNetworkStep(ctx, "network.derive_allocation",
		attribute.String("operation", "derive_allocation"),
		attribute.String("instance_id", instanceID),
	)
	alloc, err := m.deriveAllocation(deriveCtx, instanceID)
	deriveSpanEnd(err)
	if err != nil {
		return fmt.Errorf("derive allocation: %w", err)
	}
	if alloc == nil {
		// No network configured for this instance
		return nil
	}

	// 2. Get default network details (same self-healing behavior as CreateAllocation).
	networkCtx, networkSpanEnd := startNetworkStep(ctx, "network.get_default_network",
		attribute.String("operation", "get_default_network"),
		attribute.String("instance_id", instanceID),
	)
	network, err := m.getOrInitDefaultNetwork(networkCtx)
	networkSpanEnd(err)
	if err != nil {
		return err
	}

	// 3. Recreate TAP device with same name and rate limits from instance metadata
	uploadCeilBps := uploadBps * int64(m.GetUploadBurstMultiplier())
	tapCtx, tapSpanEnd := startNetworkStep(ctx, "network.create_tap",
		attribute.String("operation", "create_tap"),
		attribute.String("instance_id", instanceID),
		attribute.String("tap", alloc.TAPDevice),
		attribute.Bool("isolated", network.Isolated),
		attribute.Bool("download_rate_limit", downloadBps > 0),
		attribute.Bool("upload_rate_limit", uploadBps > 0),
	)
	err = m.createTAPDevice(tapCtx, alloc.TAPDevice, network.Bridge, network.Isolated)
	tapSpanEnd(err)
	if err != nil {
		_ = m.deleteTAPDeviceSerialized(alloc.TAPDevice, alloc.ClassID)
		return fmt.Errorf("create TAP device: %w", err)
	}
	m.recordTAPOperation(ctx, "create")

	if downloadBps > 0 || uploadBps > 0 {
		m.enqueueRateLimit(ctx, rateLimitRequest{
			instanceID:    instanceID,
			tapName:       alloc.TAPDevice,
			bridgeName:    network.Bridge,
			downloadBps:   downloadBps,
			uploadBps:     uploadBps,
			uploadCeilBps: uploadCeilBps,
		})
	} else {
		m.clearClassID(instanceID)
	}

	log.InfoContext(ctx, "recreated network for restore",
		"instance_id", instanceID,
		"network", "default",
		"tap", alloc.TAPDevice,
		"download_bps", downloadBps,
		"upload_bps", uploadBps)

	return nil
}

func (m *manager) reserveAllocationIdentityLocked(ctx context.Context, network *Network, req AllocateRequest) (*NetworkConfig, error) {
	listCtx, listSpanEnd := startNetworkStep(ctx, "network.list_allocations",
		attribute.String("operation", "list_allocations"),
		attribute.String("caller", "create_allocation"),
	)
	allocations, err := m.listAllocationsWithPendingLocked(listCtx)
	listSpanEnd(err)
	if err != nil {
		return nil, fmt.Errorf("list allocations: %w", err)
	}

	_, nameSpanEnd := startNetworkStep(ctx, "network.name_exists",
		attribute.String("operation", "name_exists"),
		attribute.String("instance_id", req.InstanceID),
	)
	exists := nameExistsInAllocations(allocations, req.InstanceName, req.InstanceID)
	nameSpanEnd(nil)
	if exists {
		return nil, fmt.Errorf("%w: instance name '%s' already exists, can't assign into same network: %s",
			ErrNameExists, req.InstanceName, network.Name)
	}

	_, ipSpanEnd := startNetworkStep(ctx, "network.allocate_ip",
		attribute.String("operation", "allocate_ip"),
		attribute.String("instance_id", req.InstanceID),
	)
	ip, err := allocateNextIPFromAllocations(network.Subnet, allocations)
	ipSpanEnd(err)
	if err != nil {
		return nil, fmt.Errorf("allocate IP: %w", err)
	}

	_, macSpanEnd := startNetworkStep(ctx, "network.allocate_mac",
		attribute.String("operation", "allocate_mac"),
		attribute.String("instance_id", req.InstanceID),
	)
	mac, err := allocateUniqueMACFromAllocations(allocations, generateMAC)
	macSpanEnd(err)
	if err != nil {
		return nil, fmt.Errorf("allocate MAC: %w", err)
	}

	tap := GenerateTAPName(req.InstanceID)
	_, ipNet, _ := net.ParseCIDR(network.Subnet)
	netmask := fmt.Sprintf("%d.%d.%d.%d", ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])

	m.rememberPendingAllocationLocked(Allocation{
		InstanceID:   req.InstanceID,
		InstanceName: req.InstanceName,
		Network:      "default",
		IP:           ip,
		MAC:          mac,
		TAPDevice:    tap,
		Gateway:      network.Gateway,
		Netmask:      netmask,
		DNS:          m.config.Network.DNSServer,
		State:        "pending",
	})

	return &NetworkConfig{
		IP:        ip,
		MAC:       mac,
		Gateway:   network.Gateway,
		Netmask:   netmask,
		DNS:       m.config.Network.DNSServer,
		TAPDevice: tap,
	}, nil
}

type rateLimitRequest struct {
	instanceID    string
	tapName       string
	bridgeName    string
	downloadBps   int64
	uploadBps     int64
	uploadCeilBps int64
}

func (m *manager) enqueueRateLimit(ctx context.Context, req rateLimitRequest) {
	go m.applyRateLimitAsync(ctx, req)
}

func (m *manager) applyRateLimitAsync(ctx context.Context, req rateLimitRequest) {
	waitCtx, waitSpanEnd := startDetachedNetworkStep(ctx, "network.rate_limit.wait_for_tc_mutex",
		attribute.String("operation", "wait_for_tc_mutex"),
		attribute.String("instance_id", req.instanceID),
		attribute.String("tap", req.tapName),
	)
	ctx = waitCtx
	log := logger.FromContext(ctx)

	m.tcMu.Lock()
	waitSpanEnd(nil)
	defer m.tcMu.Unlock()

	if !m.tapDeviceExists(req.tapName) {
		log.DebugContext(ctx, "skipping async network rate limits for released TAP",
			"instance_id", req.instanceID,
			"tap", req.tapName)
		return
	}

	applyCtx, applySpanEnd := startNetworkStep(waitCtx, "network.rate_limit.apply",
		attribute.String("operation", "apply_rate_limit"),
		attribute.String("instance_id", req.instanceID),
		attribute.String("tap", req.tapName),
		attribute.String("bridge", req.bridgeName),
		attribute.Bool("download_rate_limit", req.downloadBps > 0),
		attribute.Bool("upload_rate_limit", req.uploadBps > 0),
	)
	var applyErr error
	defer func() {
		applySpanEnd(applyErr)
	}()

	if req.downloadBps > 0 {
		downloadCtx, downloadEnd := startNetworkStep(applyCtx, "network.rate_limit.download",
			attribute.String("operation", "download_rate_limit"),
			attribute.String("instance_id", req.instanceID),
			attribute.String("tap", req.tapName),
		)
		if err := m.applyDownloadRateLimit(downloadCtx, req.tapName, req.downloadBps); err != nil {
			downloadEnd(err)
			applyErr = err
			log.ErrorContext(ctx, "failed to apply async download rate limit",
				"instance_id", req.instanceID,
				"tap", req.tapName,
				"error", err)
		} else {
			downloadEnd(nil)
		}
	}

	if req.uploadBps > 0 {
		uploadCtx, uploadEnd := startNetworkStep(applyCtx, "network.rate_limit.upload",
			attribute.String("operation", "upload_rate_limit"),
			attribute.String("instance_id", req.instanceID),
			attribute.String("tap", req.tapName),
			attribute.String("bridge", req.bridgeName),
		)
		classID, err := m.addVMClass(uploadCtx, req.bridgeName, req.tapName, req.uploadBps, req.uploadCeilBps)
		uploadEnd(err)
		if err != nil {
			applyErr = err
			log.ErrorContext(ctx, "failed to apply async upload rate limit",
				"instance_id", req.instanceID,
				"tap", req.tapName,
				"bridge", req.bridgeName,
				"error", err)
			return
		}

		_, saveEnd := startNetworkStep(applyCtx, "network.save_class_id",
			attribute.String("operation", "save_class_id"),
			attribute.String("instance_id", req.instanceID),
			attribute.Bool("has_class_id", classID != ""),
		)
		if classID != "" {
			if err := m.saveClassID(req.instanceID, classID); err != nil {
				saveEnd(err)
				applyErr = err
				log.ErrorContext(ctx, "failed to persist async tc class id",
					"instance_id", req.instanceID,
					"tap", req.tapName,
					"class_id", classID,
					"error", err)
				return
			}
		} else {
			m.clearClassID(req.instanceID)
		}
		saveEnd(nil)
	} else {
		m.clearClassID(req.instanceID)
	}

	log.DebugContext(ctx, "async network rate limits applied",
		"instance_id", req.instanceID,
		"tap", req.tapName,
		"download_bps", req.downloadBps,
		"upload_bps", req.uploadBps)
}

// ReleaseByInstanceID is a best-effort fallback for cases where the full Allocation
// can't be derived (e.g. metadata read failure during stop, or rollback after a
// CreateAllocation that succeeded but where downstream metadata writes failed).
// It deletes the TAP device using the deterministic name from the instance ID and
// the persisted class ID if available.
func (m *manager) ReleaseByInstanceID(ctx context.Context, instanceID string) error {
	return m.ReleaseAllocation(ctx, &Allocation{
		InstanceID: instanceID,
		TAPDevice:  GenerateTAPName(instanceID),
		ClassID:    m.loadClassID(instanceID),
	})
}

// ReleaseAllocation cleans up network allocation (shutdown/delete)
// Takes the allocation directly since it should be retrieved before the VMM is killed.
// If alloc is nil, this is a no-op (network not allocated or already released).
// Note: TAP devices created with explicit Owner/Group fields do NOT auto-delete when
// the process closes the file descriptor. They persist in the kernel and must be
// explicitly deleted via this function. In case of unexpected scenarios like host
// power loss, straggler TAP devices may remain until the host is rebooted or manually cleaned up.
func (m *manager) ReleaseAllocation(ctx context.Context, alloc *Allocation) error {
	log := logger.FromContext(ctx)

	// If no allocation provided, nothing to clean up
	if alloc == nil {
		return nil
	}
	m.forgetPendingAllocation(alloc.InstanceID)

	// 1. Delete TAP device (best effort), using stored class ID for correct HTB cleanup.
	// Serialize with async tc setup so queued rate-limit work cannot race with
	// class removal and leave stale bridge state behind.
	if err := m.deleteTAPDeviceForInstanceSerialized(alloc.InstanceID, alloc.TAPDevice, alloc.ClassID); err != nil {
		log.WarnContext(ctx, "failed to delete TAP device", "tap", alloc.TAPDevice, "error", err)
	} else {
		m.recordTAPOperation(ctx, "delete")
	}

	log.InfoContext(ctx, "released network",
		"instance_id", alloc.InstanceID,
		"network", alloc.Network,
		"ip", alloc.IP)

	return nil
}

func (m *manager) deleteTAPDeviceSerialized(tapName, classID string) error {
	m.tcMu.Lock()
	defer m.tcMu.Unlock()
	return m.deleteTAPDevice(tapName, classID)
}

func (m *manager) deleteTAPDeviceForInstanceSerialized(instanceID, tapName, classID string) error {
	m.tcMu.Lock()
	defer m.tcMu.Unlock()
	return m.deleteTAPDevice(tapName, m.classIDForDelete(instanceID, classID))
}

func (m *manager) classIDForDelete(instanceID, fallback string) string {
	if instanceID == "" {
		return fallback
	}
	if classID := m.loadClassID(instanceID); classID != "" {
		return classID
	}
	return fallback
}

// getOrInitDefaultNetwork resolves the default network and self-heals by running
// Initialize if bridge state is missing, then retries briefly to absorb netlink propagation delay.
func (m *manager) getOrInitDefaultNetwork(ctx context.Context) (*Network, error) {
	if network := m.cachedDefaultNetwork(); network != nil {
		return network, nil
	}

	network, err := m.getDefaultNetwork(ctx)
	if err == nil {
		m.setDefaultNetwork(network)
		return network, nil
	}

	// Self-heal should never delete TAPs for active instances. We pass nil so
	// CleanupOrphanedTAPs is skipped in Initialize.
	if initErr := m.Initialize(ctx, nil); initErr != nil {
		return nil, fmt.Errorf("initialize network manager: %w", initErr)
	}

	const retries = 20
	const retryDelay = 100 * time.Millisecond
	for i := 0; i < retries; i++ {
		network, err = m.getDefaultNetwork(ctx)
		if err == nil {
			m.setDefaultNetwork(network)
			return network, nil
		}
		time.Sleep(retryDelay)
	}

	return nil, fmt.Errorf("get default network after initialize: %w", err)
}

// allocateNextIP picks a random available IP in the subnet
// Retries up to 5 times if conflicts occur
func (m *manager) allocateNextIP(ctx context.Context, subnet string) (string, error) {
	chooseCtx, chooseSpanEnd := startNetworkStep(ctx, "network.allocate_ip",
		attribute.String("operation", "allocate_ip"),
	)
	var chooseErr error
	defer func() {
		chooseSpanEnd(chooseErr)
	}()

	// Get all currently allocated IPs
	listCtx, listSpanEnd := startNetworkStep(chooseCtx, "network.list_allocations",
		attribute.String("operation", "list_allocations"),
		attribute.String("caller", "allocate_ip"),
	)
	allocations, err := m.ListAllocations(listCtx)
	listSpanEnd(err)
	if err != nil {
		chooseErr = err
		return "", fmt.Errorf("list allocations: %w", err)
	}

	ip, err := allocateNextIPFromAllocations(subnet, allocations)
	if err != nil {
		chooseErr = err
	}
	return ip, err
}

func allocateNextIPFromAllocations(subnet string, allocations []Allocation) (string, error) {
	// Parse subnet
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("parse subnet: %w", err)
	}

	// Build set of used IPs
	usedIPs := make(map[string]bool)
	for _, alloc := range allocations {
		usedIPs[alloc.IP] = true
	}

	// Reserve network address and gateway
	usedIPs[ipNet.IP.String()] = true                 // Network address
	usedIPs[incrementIP(ipNet.IP, 1).String()] = true // Gateway (network + 1)

	// Calculate broadcast address
	broadcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		broadcast[i] = ipNet.IP[i] | ^ipNet.Mask[i]
	}
	usedIPs[broadcast.String()] = true // Broadcast address

	// Calculate subnet size (number of possible IPs)
	ones, bits := ipNet.Mask.Size()
	subnetSize := 1 << (bits - ones) // 2^(32-prefix_length)

	// Try up to 5 times to find a random available IP
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Generate random offset from network address (skip network and gateway)
		// Start from offset 2 to avoid network address (0) and gateway (1)
		randomOffset := mathrand.Intn(subnetSize-3) + 2

		// Calculate the random IP
		randomIP := incrementIP(ipNet.IP, randomOffset)

		// Check if IP is valid and available
		if ipNet.Contains(randomIP) {
			ipStr := randomIP.String()
			if !usedIPs[ipStr] {
				return ipStr, nil
			}
		}
	}

	// If random allocation failed after 5 attempts, fall back to sequential search
	// This handles the case where the subnet is nearly full
	for testIP := incrementIP(ipNet.IP, 2); ipNet.Contains(testIP); testIP = incrementIP(testIP, 1) {
		ipStr := testIP.String()
		if !usedIPs[ipStr] {
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("no available IPs in subnet %s after %d random attempts and full scan", subnet, maxRetries)
}

// incrementIP increments IP address by n
func incrementIP(ip net.IP, n int) net.IP {
	// Ensure we're working with IPv4 (4 bytes)
	ip4 := ip.To4()
	if ip4 == nil {
		// Should not happen with our subnet parsing, but handle it
		return ip
	}

	result := make(net.IP, 4)
	copy(result, ip4)

	// Convert to 32-bit integer, increment, convert back
	val := uint32(result[0])<<24 | uint32(result[1])<<16 | uint32(result[2])<<8 | uint32(result[3])
	val += uint32(n)
	result[0] = byte(val >> 24)
	result[1] = byte(val >> 16)
	result[2] = byte(val >> 8)
	result[3] = byte(val)

	return result
}

const (
	macAllocationRandomAttempts = 5
	macSuffixSpace              = 1 << 24
)

// allocateUniqueMAC picks an unused locally administered MAC address.
func (m *manager) allocateUniqueMAC(ctx context.Context) (string, error) {
	chooseCtx, chooseSpanEnd := startNetworkStep(ctx, "network.allocate_mac",
		attribute.String("operation", "allocate_mac"),
	)
	var chooseErr error
	defer func() {
		chooseSpanEnd(chooseErr)
	}()

	listCtx, listSpanEnd := startNetworkStep(chooseCtx, "network.list_allocations",
		attribute.String("operation", "list_allocations"),
		attribute.String("caller", "allocate_mac"),
	)
	allocations, err := m.ListAllocations(listCtx)
	listSpanEnd(err)
	if err != nil {
		chooseErr = err
		return "", fmt.Errorf("list allocations: %w", err)
	}

	mac, err := allocateUniqueMACFromAllocations(allocations, generateMAC)
	if err != nil {
		chooseErr = err
	}
	return mac, err
}

func allocateUniqueMACFromAllocations(allocations []Allocation, generate func() (string, error)) (string, error) {
	usedMACs := make(map[string]bool)
	for _, alloc := range allocations {
		mac := strings.ToLower(strings.TrimSpace(alloc.MAC))
		if mac != "" {
			usedMACs[mac] = true
		}
	}

	return allocateUniqueMACFromSet(usedMACs, generate)
}

func allocateUniqueMACFromSet(usedMACs map[string]bool, generate func() (string, error)) (string, error) {
	for attempt := 0; attempt < macAllocationRandomAttempts; attempt++ {
		mac, err := generate()
		if err != nil {
			return "", err
		}
		mac = strings.ToLower(strings.TrimSpace(mac))
		if !usedMACs[mac] {
			return mac, nil
		}
	}

	for suffix := 0; suffix < macSuffixSpace; suffix++ {
		mac := formatMACSuffix(suffix)
		if !usedMACs[mac] {
			return mac, nil
		}
	}

	return "", fmt.Errorf("no available MAC addresses after %d random attempts and full scan", macAllocationRandomAttempts)
}

// generateMAC generates a random MAC address with local administration bit set
func generateMAC() (string, error) {
	// Generate 6 random bytes
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	// Set local administration bit (bit 1 of first byte)
	// Use 02:00:00:... format (locally administered, unicast)
	buf[0] = 0x02
	buf[1] = 0x00
	buf[2] = 0x00

	// Format as MAC address
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		buf[0], buf[1], buf[2], buf[3], buf[4], buf[5]), nil
}

func formatMACSuffix(suffix int) string {
	return fmt.Sprintf("02:00:00:%02x:%02x:%02x",
		byte(suffix>>16), byte(suffix>>8), byte(suffix))
}

// saveClassID persists the tc class ID for an instance so it survives restarts.
func (m *manager) saveClassID(instanceID, classID string) error {
	return os.WriteFile(m.paths.InstanceDir(instanceID)+"/classid", []byte(classID), 0644)
}

// clearClassID removes any persisted tc class ID for an instance.
func (m *manager) clearClassID(instanceID string) {
	_ = os.Remove(m.paths.InstanceDir(instanceID) + "/classid")
}

// loadClassID loads the persisted tc class ID for an instance.
// Returns empty string if not found (backwards compat for old allocations).
func (m *manager) loadClassID(instanceID string) string {
	path := m.paths.InstanceDir(instanceID)
	data, err := os.ReadFile(path + "/classid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// TAPPrefix is the prefix used for hypeman TAP devices
const TAPPrefix = "hype-"

// GenerateTAPName generates TAP device name from instance ID.
// Exported for use by other packages (e.g., vm_metrics).
func GenerateTAPName(instanceID string) string {
	// Use first 8 chars of instance ID
	// hype-{8chars} fits within 15-char Linux interface name limit
	shortID := instanceID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return TAPPrefix + strings.ToLower(shortID)
}
