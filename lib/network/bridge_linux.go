//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/vishvananda/netlink"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sys/unix"
)

const netlinkDumpRetryCount = 3
const iptablesWaitSeconds = "5"

func newIPTablesCommand(args ...string) *exec.Cmd {
	fullArgs := make([]string, 0, len(args)+2)
	fullArgs = append(fullArgs, "-w", iptablesWaitSeconds)
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command("iptables", fullArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	return cmd
}

func listBridgeAddrsWithRetry(link netlink.Link) ([]netlink.Addr, error) {
	var err error
	for i := 0; i < netlinkDumpRetryCount; i++ {
		addrs, listErr := netlink.AddrList(link, netlink.FAMILY_V4)
		if listErr == nil {
			return addrs, nil
		}
		if !errors.Is(listErr, netlink.ErrDumpInterrupted) {
			return nil, listErr
		}
		err = listErr
		time.Sleep(10 * time.Millisecond)
	}
	return nil, err
}

// checkSubnetConflicts checks if the configured subnet conflicts with existing routes.
// Returns an error if a conflict is detected, with guidance on how to resolve it.
func (m *manager) checkSubnetConflicts(ctx context.Context, subnet string) error {
	log := logger.FromContext(ctx)

	_, configuredNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("parse subnet: %w", err)
	}

	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list routes: %w", err)
	}

	for _, route := range routes {
		if route.Dst == nil {
			continue // Skip default route (nil Dst)
		}

		// Skip default route (0.0.0.0/0) - it matches everything but isn't a real conflict
		if route.Dst.IP.IsUnspecified() {
			continue
		}

		// Check if our subnet overlaps with this route's destination
		// Overlap occurs if either network contains the other's start address
		if configuredNet.Contains(route.Dst.IP) || route.Dst.Contains(configuredNet.IP) {
			// Get interface name for better error message
			ifaceName := "unknown"
			if link, err := netlink.LinkByIndex(route.LinkIndex); err == nil {
				ifaceName = link.Attrs().Name
			}

			// Skip if this is our own bridge (already configured from previous run)
			if ifaceName == m.config.Network.BridgeName {
				continue
			}

			log.ErrorContext(ctx, "subnet conflict detected",
				"configured_subnet", subnet,
				"conflicting_route", route.Dst.String(),
				"interface", ifaceName)

			return fmt.Errorf("SUBNET CONFLICT: configured subnet %s overlaps with existing route %s (interface: %s)\n\n"+
				"This will cause network connectivity issues. Please update your configuration:\n"+
				"  - Set SUBNET_CIDR to a non-conflicting range (e.g., 10.200.0.0/16, 172.30.0.0/16)\n"+
				"  - Set SUBNET_GATEWAY to match (e.g., 10.200.0.1, 172.30.0.1)\n\n"+
				"To see existing routes: ip route show",
				subnet, route.Dst.String(), ifaceName)
		}
	}

	log.DebugContext(ctx, "no subnet conflicts detected", "subnet", subnet)
	return nil
}

// createBridge creates or verifies a bridge interface using netlink
func (m *manager) createBridge(ctx context.Context, name, gateway, subnet string) error {
	log := logger.FromContext(ctx)

	// 1. Parse subnet to get network and prefix length
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("parse subnet: %w", err)
	}

	// 2. Check if bridge already exists
	existing, err := netlink.LinkByName(name)
	if err == nil {
		// Bridge exists - verify it has the expected gateway IP
		addrs, err := listBridgeAddrsWithRetry(existing)
		if err != nil {
			return fmt.Errorf("list bridge addresses: %w", err)
		}

		expectedGW := net.ParseIP(gateway)
		hasExpectedIP := false
		var actualIPs []string
		for _, addr := range addrs {
			actualIPs = append(actualIPs, addr.IPNet.String())
			if addr.IP.Equal(expectedGW) {
				hasExpectedIP = true
			}
		}

		if !hasExpectedIP {
			ones, _ := ipNet.Mask.Size()
			return fmt.Errorf("bridge %s exists with IPs %v but expected gateway %s/%d. "+
				"Options: (1) update SUBNET_CIDR and SUBNET_GATEWAY to match the existing bridge, "+
				"(2) use a different BRIDGE_NAME, "+
				"or (3) delete the bridge with: sudo ip link delete %s",
				name, actualIPs, gateway, ones, name)
		}

		// Bridge exists with correct IP, verify it's up
		if err := netlink.LinkSetUp(existing); err != nil {
			return fmt.Errorf("set bridge up: %w", err)
		}
		log.InfoContext(ctx, "bridge ready", "bridge", name, "gateway", gateway, "status", "existing")

		// Still need to ensure iptables rules are configured
		if err := m.setupIPTablesRules(ctx, subnet, name); err != nil {
			return fmt.Errorf("setup iptables: %w", err)
		}
		return nil
	}

	// 3. Create bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
	}

	if err := netlink.LinkAdd(bridge); err != nil {
		return fmt.Errorf("create bridge: %w", err)
	}

	// 4. Set bridge up
	if err := netlink.LinkSetUp(bridge); err != nil {
		return fmt.Errorf("set bridge up: %w", err)
	}

	// 5. Add gateway IP to bridge
	gatewayIP := net.ParseIP(gateway)
	if gatewayIP == nil {
		return fmt.Errorf("invalid gateway IP: %s", gateway)
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   gatewayIP,
			Mask: ipNet.Mask,
		},
	}

	if err := netlink.AddrAdd(bridge, addr); err != nil {
		return fmt.Errorf("add gateway IP to bridge: %w", err)
	}

	log.InfoContext(ctx, "bridge ready", "bridge", name, "gateway", gateway, "status", "created")

	// 6. Setup iptables rules
	if err := m.setupIPTablesRules(ctx, subnet, name); err != nil {
		return fmt.Errorf("setup iptables: %w", err)
	}

	return nil
}

// Rule comments for identifying hypeman iptables rules
const (
	commentNATBase    = "hypeman-nat"
	commentFwdOutBase = "hypeman-fwd-out"
	commentFwdInBase  = "hypeman-fwd-in"
)

// HTB handles for traffic control
const (
	htbRootHandle  = "1:"  // Root qdisc handle
	htbRootClassID = "1:1" // Root class for total capacity
)

// getUplinkInterface returns the uplink interface for NAT/forwarding.
// Uses explicit config if set, otherwise auto-detects from default route.
func (m *manager) getUplinkInterface() (string, error) {
	// Explicit config takes precedence
	if m.config.Network.UplinkInterface != "" {
		return m.config.Network.UplinkInterface, nil
	}

	// Auto-detect from default route
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}

	for _, route := range routes {
		// Default route has Dst 0.0.0.0/0 (IP.IsUnspecified() == true)
		if route.Dst != nil && route.Dst.IP.IsUnspecified() {
			link, err := netlink.LinkByIndex(route.LinkIndex)
			if err != nil {
				return "", fmt.Errorf("get link by index %d: %w", route.LinkIndex, err)
			}
			return link.Attrs().Name, nil
		}
	}

	return "", fmt.Errorf("no default route found - cannot determine uplink interface")
}

// setupIPTablesRules sets up NAT and forwarding rules
func (m *manager) setupIPTablesRules(ctx context.Context, subnet, bridgeName string) error {
	log := logger.FromContext(ctx)

	// Check if IP forwarding is enabled (prerequisite)
	forwardData, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return fmt.Errorf("check ip forwarding: %w", err)
	}
	if strings.TrimSpace(string(forwardData)) != "1" {
		return fmt.Errorf("IPv4 forwarding is not enabled. Please enable it by running: sudo sysctl -w net.ipv4.ip_forward=1 (or add 'net.ipv4.ip_forward=1' to /etc/sysctl.conf for persistence)")
	}
	log.InfoContext(ctx, "ip forwarding enabled")

	// Get uplink interface (explicit config or auto-detect from default route)
	uplink, err := m.getUplinkInterface()
	if err != nil {
		return fmt.Errorf("get uplink interface: %w", err)
	}
	log.InfoContext(ctx, "uplink interface", "interface", uplink)

	natComment := m.ruleComment(commentNATBase)
	fwdOutComment := m.ruleComment(commentFwdOutBase)
	fwdInComment := m.ruleComment(commentFwdInBase)

	// Add MASQUERADE rule if not exists (position doesn't matter in POSTROUTING)
	masqStatus, err := m.ensureNATRule(subnet, uplink, natComment)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "iptables NAT ready", "subnet", subnet, "uplink", uplink, "status", masqStatus)

	// FORWARD rules must be at top of chain (before Docker's DOCKER-USER/DOCKER-FORWARD)
	// We insert at position 1 and 2 to ensure they're evaluated first
	fwdOutStatus, err := m.ensureForwardRule(bridgeName, uplink, "NEW,ESTABLISHED,RELATED", fwdOutComment, 1)
	if err != nil {
		return fmt.Errorf("setup forward outbound: %w", err)
	}

	fwdInStatus, err := m.ensureForwardRule(uplink, bridgeName, "ESTABLISHED,RELATED", fwdInComment, 2)
	if err != nil {
		return fmt.Errorf("setup forward inbound: %w", err)
	}

	log.InfoContext(ctx, "iptables FORWARD ready", "outbound", fwdOutStatus, "inbound", fwdInStatus)

	// Restore Docker's FORWARD chain jumps if they were lost.
	// On systems where an external tool (e.g., hypervisor firewall management) periodically
	// rebuilds the FORWARD chain, Docker's jump rules can be wiped out. Docker only inserts
	// them at daemon start, so they stay missing until Docker is restarted. Since hypeman
	// already re-ensures its own rules here, we also restore Docker's if needed.
	m.ensureDockerForwardJump(ctx)

	return nil
}

// ensureNATRule ensures the MASQUERADE rule exists with correct uplink
func (m *manager) ensureNATRule(subnet, uplink, comment string) (string, error) {
	// Check if rule exists with correct subnet and uplink
	checkCmd := newIPTablesCommand("-t", "nat", "-C", "POSTROUTING",
		"-s", subnet, "-o", uplink,
		"-m", "comment", "--comment", comment,
		"-j", "MASQUERADE")
	if checkCmd.Run() == nil {
		return "existing", nil
	}

	// Delete any existing rule with our comment (handles uplink changes)
	m.deleteNATRuleByComment(comment)

	// Add rule with comment
	addCmd := newIPTablesCommand("-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "-o", uplink,
		"-m", "comment", "--comment", comment,
		"-j", "MASQUERADE")
	if err := addCmd.Run(); err != nil {
		return "", fmt.Errorf("add masquerade rule: %w", err)
	}
	return "added", nil
}

// ruleComment returns a bridge-scoped iptables comment so concurrent managers
// don't clobber each other's rules.
func (m *manager) ruleComment(base string) string {
	suffix := strings.ToLower(m.config.Network.BridgeName)
	suffix = strings.ReplaceAll(suffix, " ", "-")
	return fmt.Sprintf("%s-%s", base, suffix)
}

// deleteNATRuleByComment deletes any NAT POSTROUTING rule containing our comment
func (m *manager) deleteNATRuleByComment(comment string) {
	// List NAT POSTROUTING rules
	cmd := newIPTablesCommand("-t", "nat", "-L", "POSTROUTING", "--line-numbers", "-n")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	// Find rule numbers with our comment (process in reverse to avoid renumbering issues)
	var ruleNums []string
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, comment) {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				ruleNums = append(ruleNums, fields[0])
			}
		}
	}

	// Delete in reverse order
	for i := len(ruleNums) - 1; i >= 0; i-- {
		delCmd := newIPTablesCommand("-t", "nat", "-D", "POSTROUTING", ruleNums[i])
		delCmd.Run() // ignore error
	}
}

// ensureForwardRule ensures a FORWARD rule exists at the correct position with correct interfaces
func (m *manager) ensureForwardRule(inIface, outIface, ctstate, comment string, position int) (string, error) {
	// Check if rule exists at correct position with correct interfaces
	if m.isForwardRuleCorrect(inIface, outIface, comment, position) {
		return "existing", nil
	}

	// Delete any existing rule with our comment (handles interface/position changes)
	m.deleteForwardRuleByComment(comment)

	// Insert at specified position with comment
	addCmd := newIPTablesCommand("-I", "FORWARD", fmt.Sprintf("%d", position),
		"-i", inIface, "-o", outIface,
		"-m", "conntrack", "--ctstate", ctstate,
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT")
	if err := addCmd.Run(); err != nil {
		return "", fmt.Errorf("insert forward rule: %w", err)
	}
	return "added", nil
}

// isForwardRuleCorrect checks if our rule exists at the expected position with correct interfaces
func (m *manager) isForwardRuleCorrect(inIface, outIface, comment string, position int) bool {
	// List FORWARD chain with line numbers
	cmd := newIPTablesCommand("-L", "FORWARD", "--line-numbers", "-n", "-v")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Look for our comment at the expected position with correct interfaces
	// Line format: "1    0     0 ACCEPT  0    --  vmbr0  eth0   0.0.0.0/0  0.0.0.0/0  ... /* hypeman-fwd-out */"
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if !strings.Contains(line, comment) {
			continue
		}
		fields := strings.Fields(line)
		// Check position (field 0), in interface (field 6), out interface (field 7)
		if len(fields) >= 8 &&
			fields[0] == fmt.Sprintf("%d", position) &&
			fields[6] == inIface &&
			fields[7] == outIface {
			return true
		}
	}
	return false
}

// deleteForwardRuleByComment deletes any FORWARD rule containing our comment
func (m *manager) deleteForwardRuleByComment(comment string) {
	// List FORWARD rules
	cmd := newIPTablesCommand("-L", "FORWARD", "--line-numbers", "-n")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	// Find rule numbers with our comment (process in reverse to avoid renumbering issues)
	var ruleNums []string
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, comment) {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				ruleNums = append(ruleNums, fields[0])
			}
		}
	}

	// Delete in reverse order
	for i := len(ruleNums) - 1; i >= 0; i-- {
		delCmd := newIPTablesCommand("-D", "FORWARD", ruleNums[i])
		delCmd.Run() // ignore error
	}
}

// ensureDockerForwardJump checks if Docker's DOCKER-FORWARD chain exists but is
// unreachable from the FORWARD chain, and restores the jump if missing.
// This is a no-op if Docker is not installed or the jump already exists.
//
// Note: this cannot mis-order DOCKER-FORWARD vs DOCKER-USER because it only acts
// when the jump is completely absent (chain was flushed). If DOCKER-USER's jump
// still exists, DOCKER-FORWARD's jump is almost certainly still there too — they
// get wiped together — and the early -C check returns before we insert anything.
func (m *manager) ensureDockerForwardJump(ctx context.Context) {
	log := logger.FromContext(ctx)

	// Check if DOCKER-FORWARD chain exists (Docker is installed and configured)
	checkChain := newIPTablesCommand("-L", "DOCKER-FORWARD", "-n")
	if checkChain.Run() != nil {
		return // Chain doesn't exist — Docker not installed or not configured
	}

	// Check if jump already exists in FORWARD
	checkJump := newIPTablesCommand("-C", "FORWARD", "-j", "DOCKER-FORWARD")
	if checkJump.Run() == nil {
		return // Jump already present
	}

	// DOCKER-FORWARD chain exists but the jump from FORWARD is missing — restore it.
	// Insert right after hypeman's last rule so the jump is evaluated before any
	// explicit DROP/REJECT rules that an external firewall tool may have added.
	insertPos := m.lastHypemanForwardRulePosition() + 1
	addJump := newIPTablesCommand("-I", "FORWARD", fmt.Sprintf("%d", insertPos), "-j", "DOCKER-FORWARD")
	if err := addJump.Run(); err != nil {
		log.WarnContext(ctx, "failed to restore Docker FORWARD chain jump", "error", err)
		return
	}

	log.WarnContext(ctx, "restored missing jump to DOCKER-FORWARD in FORWARD chain", "position", insertPos)
}

// lastHypemanForwardRulePosition returns the line number of the last hypeman-managed
// rule in the FORWARD chain, or 0 if none are found.
func (m *manager) lastHypemanForwardRulePosition() int {
	cmd := newIPTablesCommand("-L", "FORWARD", "--line-numbers", "-n", "-v")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	lastPos := 0
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, "hypeman-") {
			continue
		}
		var pos int
		if _, err := fmt.Sscanf(line, "%d", &pos); err == nil && pos > lastPos {
			lastPos = pos
		}
	}
	return lastPos
}

// createTAPDevice creates TAP device and attaches it to the bridge.
func (m *manager) createTAPDevice(ctx context.Context, tapName, bridgeName string, isolated bool) error {
	// 1. Check if TAP already exists
	_, linkLookupEnd := startNetworkStep(ctx, "network.create_tap.link_lookup_existing",
		attribute.String("operation", "link_lookup_existing"),
		attribute.String("tap", tapName),
	)
	_, err := netlink.LinkByName(tapName)
	linkLookupEnd(nil)
	if err == nil {
		// TAP already exists, delete it first
		_, deleteEnd := startNetworkStep(ctx, "network.create_tap.delete_existing",
			attribute.String("operation", "delete_existing"),
			attribute.String("tap", tapName),
		)
		err := m.deleteTAPDeviceSerialized(tapName)
		deleteEnd(err)
		if err != nil {
			return fmt.Errorf("delete existing TAP: %w", err)
		}
	}

	// 2. Create TAP device with current user as owner
	// This allows Cloud Hypervisor (running as current user) to access the TAP
	uid := os.Getuid()
	gid := os.Getgid()

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: tapName,
		},
		Mode:  netlink.TUNTAP_MODE_TAP,
		Owner: uint32(uid),
		Group: uint32(gid),
	}

	_, linkAddEnd := startNetworkStep(ctx, "network.create_tap.link_add",
		attribute.String("operation", "link_add"),
		attribute.String("tap", tapName),
	)
	err = netlink.LinkAdd(tap)
	linkAddEnd(err)
	if err != nil {
		return fmt.Errorf("create TAP device: %w", err)
	}

	// 3. Set TAP up
	tapLink := tap

	_, setUpEnd := startNetworkStep(ctx, "network.create_tap.link_set_up",
		attribute.String("operation", "link_set_up"),
		attribute.String("tap", tapName),
	)
	err = netlink.LinkSetUp(tapLink)
	setUpEnd(err)
	if err != nil {
		return fmt.Errorf("set TAP up: %w", err)
	}

	// 4. Attach TAP to bridge
	_, bridgeLookupEnd := startNetworkStep(ctx, "network.create_tap.link_lookup_bridge",
		attribute.String("operation", "link_lookup_bridge"),
		attribute.String("bridge", bridgeName),
	)
	bridge, err := netlink.LinkByName(bridgeName)
	bridgeLookupEnd(err)
	if err != nil {
		return fmt.Errorf("get bridge: %w", err)
	}

	_, setMasterEnd := startNetworkStep(ctx, "network.create_tap.link_set_master",
		attribute.String("operation", "link_set_master"),
		attribute.String("tap", tapName),
		attribute.String("bridge", bridgeName),
	)
	err = netlink.LinkSetMaster(tapLink, bridge)
	setMasterEnd(err)
	if err != nil {
		return fmt.Errorf("attach TAP to bridge: %w", err)
	}

	// 5. Enable port isolation so isolated TAPs can't directly talk to each other (requires kernel support and capabilities)
	if isolated {
		_, isolationEnd := startNetworkStep(ctx, "network.create_tap.set_isolation",
			attribute.String("operation", "set_isolation"),
			attribute.String("tap", tapName),
		)
		err = netlink.LinkSetIsolated(tapLink, true)
		isolationEnd(err)
		if err != nil {
			return fmt.Errorf("set isolation mode: %w", err)
		}
	}

	return nil
}

// applyDownloadRateLimit applies download (external→VM) rate limiting using TBF on TAP egress.
func (m *manager) applyDownloadRateLimit(ctx context.Context, tapName string, rateLimitBps int64) error {
	rateStr := formatTcRate(rateLimitBps)

	// Use Token Bucket Filter (tbf) for download shaping
	// burst: bucket size = (rate * multiplier) / 250 for HZ=250 kernels
	// The multiplier allows initial burst before settling to sustained rate.
	// latency: max time a packet can wait in queue
	multiplier := m.GetDownloadBurstMultiplier()
	burstBytes := (rateLimitBps * int64(multiplier)) / 250
	if burstBytes < 1540 {
		burstBytes = 1540 // Minimum burst for standard MTU
	}

	cmd := exec.Command("tc", "qdisc", "add", "dev", tapName, "root", "tbf",
		"rate", rateStr,
		"burst", fmt.Sprintf("%d", burstBytes),
		"latency", "50ms")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	_, tcEnd := startNetworkStep(ctx, "network.rate_limit.download.tc_qdisc_tbf",
		attribute.String("operation", "tc_qdisc_tbf"),
		attribute.String("tap", tapName),
	)
	output, err := cmd.CombinedOutput()
	tcEnd(err)
	if err != nil {
		return fmt.Errorf("tc qdisc add tbf: %w (output: %s)", err, string(output))
	}

	return nil
}

// removeRateLimit removes any rate limiting from a TAP device.
func (m *manager) removeRateLimit(tapName string) error {
	cmd := exec.Command("tc", "qdisc", "del", "dev", tapName, "root")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	// Ignore errors - qdisc may not exist
	cmd.Run()
	return nil
}

// setupBridgeHTB sets up HTB qdisc on bridge for upload (VM→external) fair sharing.
// This is one-time setup - per-VM classes are added dynamically via addVMClass.
func (m *manager) setupBridgeHTB(ctx context.Context, bridgeName string, capacityBps int64) error {
	log := logger.FromContext(ctx)

	if capacityBps <= 0 {
		log.DebugContext(ctx, "skipping HTB setup - no capacity configured", "bridge", bridgeName)
		return nil
	}

	// Check if HTB qdisc already exists
	checkCmd := exec.Command("tc", "qdisc", "show", "dev", bridgeName)
	checkCmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	output, err := checkCmd.Output()
	if err == nil && strings.Contains(string(output), "htb") {
		log.InfoContext(ctx, "HTB qdisc ready", "bridge", bridgeName, "status", "existing")
		return nil
	}

	rateStr := formatTcRate(capacityBps)

	// 1. Add root HTB qdisc (no default - all traffic must be classified)
	cmd := exec.Command("tc", "qdisc", "add", "dev", bridgeName, "root",
		"handle", htbRootHandle, "htb")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc qdisc add htb: %w (output: %s)", err, string(output))
	}

	// 2. Add root class for total capacity
	cmd = exec.Command("tc", "class", "add", "dev", bridgeName, "parent", htbRootHandle,
		"classid", htbRootClassID, "htb", "rate", rateStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc class add root: %w (output: %s)", err, string(output))
	}

	log.InfoContext(ctx, "HTB qdisc ready", "bridge", bridgeName, "capacity", rateStr, "status", "configured")
	return nil
}

// addVMClass adds an HTB class for a VM on the bridge for upload rate limiting.
// Called during TAP device creation. rateBps is guaranteed, ceilBps is burst ceiling.
// Returns the class ID actually used (may differ from deriveClassID if a collision occurred).
func (m *manager) addVMClass(ctx context.Context, bridgeName, tapName string, rateBps, ceilBps int64) (string, error) {
	if rateBps <= 0 {
		return "", nil // No rate limiting configured
	}

	rateStr := formatTcRate(rateBps)
	if ceilBps <= 0 {
		ceilBps = rateBps
	}
	ceilStr := formatTcRate(ceilBps)

	// Start with derived class ID, probe linearly on collision.
	classIDVal := deriveClassIDVal(tapName)

	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		classID := fmt.Sprintf("%04x", classIDVal)
		fullClassID := fmt.Sprintf("1:%s", classID)

		// Try tc class add (NOT replace) so we detect collisions.
		cmd := exec.Command("tc", "class", "add", "dev", bridgeName, "parent", htbRootClassID,
			"classid", fullClassID, "htb", "rate", rateStr, "ceil", ceilStr, "prio", "1")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		_, classAddEnd := startNetworkStep(ctx, "network.rate_limit.upload.tc_class_add",
			attribute.String("operation", "tc_class_add"),
			attribute.String("tap", tapName),
			attribute.String("bridge", bridgeName),
			attribute.String("class_id", fullClassID),
			attribute.Int("attempt", attempt+1),
		)
		output, err := cmd.CombinedOutput()
		classAddEnd(err)
		if err != nil {
			// Check for "File exists" collision (exit status 2).
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 && strings.Contains(string(output), "File exists") {
				if attempt == 0 {
					m.recordTCClassCollision(ctx, "initial")
				} else {
					m.recordTCClassCollision(ctx, "retry")
				}
				// Increment class ID, wrapping within valid 16-bit range.
				// Skip 0 (invalid) and 1 (root class 1:1).
				classIDVal++
				if classIDVal == 0 || classIDVal == 1 {
					classIDVal = 2
				}
				lastErr = fmt.Errorf("tc class add: %w (output: %s)", err, string(output))
				continue
			}
			// Non-collision error: return immediately.
			return "", fmt.Errorf("tc class add vm: %w (output: %s)", err, string(output))
		}

		// Success — add fq_codel and filter.
		qdiscCmd := exec.Command("tc", "qdisc", "add", "dev", bridgeName, "parent", fullClassID, "fq_codel")
		qdiscCmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		_, fqCodelEnd := startNetworkStep(ctx, "network.rate_limit.upload.tc_qdisc_fq_codel",
			attribute.String("operation", "tc_qdisc_fq_codel"),
			attribute.String("tap", tapName),
			attribute.String("bridge", bridgeName),
			attribute.String("class_id", fullClassID),
		)
		fqCodelErr := qdiscCmd.Run() // Best effort
		fqCodelEnd(fqCodelErr)

		_, filterLinkEnd := startNetworkStep(ctx, "network.rate_limit.upload.link_lookup_filter",
			attribute.String("operation", "link_lookup_filter"),
			attribute.String("tap", tapName),
		)
		tapLink, linkErr := netlink.LinkByName(tapName)
		filterLinkEnd(linkErr)
		if linkErr != nil {
			return "", fmt.Errorf("get TAP link for filter: %w", linkErr)
		}
		tapIndex := tapLink.Attrs().Index

		filterCmd := exec.Command("tc", "filter", "add", "dev", bridgeName, "parent", htbRootHandle,
			"protocol", "all", "prio", "1", "basic",
			"match", fmt.Sprintf("meta(rt_iif eq %d)", tapIndex),
			"flowid", fullClassID)
		filterCmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		_, filterEnd := startNetworkStep(ctx, "network.rate_limit.upload.tc_filter_add",
			attribute.String("operation", "tc_filter_add"),
			attribute.String("tap", tapName),
			attribute.String("bridge", bridgeName),
			attribute.String("class_id", fullClassID),
		)
		output, filterErr := filterCmd.CombinedOutput()
		filterEnd(filterErr)
		if filterErr != nil {
			return "", fmt.Errorf("tc filter add: %w (output: %s)", filterErr, string(output))
		}

		return classID, nil
	}

	return "", fmt.Errorf("tc class add failed after %d attempts: %w", maxAttempts, lastErr)
}

type bridgeFilter struct {
	handle string
	flowID string
	rtIif  int
}

func parseBridgeFilters(output string) []bridgeFilter {
	var filters []bridgeFilter
	var current bridgeFilter
	haveCurrent := false

	flush := func() {
		if haveCurrent && current.handle != "" && current.flowID != "" {
			filters = append(filters, current)
		}
	}

	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "filter ") {
			flush()
			current = bridgeFilter{rtIif: -1}
			haveCurrent = true

			fields := strings.Fields(line)
			for i, field := range fields {
				if i+1 >= len(fields) {
					break
				}
				switch field {
				case "handle":
					current.handle = fields[i+1]
				case "flowid":
					current.flowID = fields[i+1]
				}
			}
			continue
		}

		if !haveCurrent {
			continue
		}
		if idx := strings.Index(line, "rt_iif eq "); idx >= 0 {
			rest := line[idx+len("rt_iif eq "):]
			if end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
				rest = rest[:end]
			}
			if rtIif, err := strconv.Atoi(rest); err == nil {
				current.rtIif = rtIif
			}
		}
	}
	flush()

	return filters
}

func listBridgeFilters(bridgeName string) ([]bridgeFilter, error) {
	cmd := exec.Command("tc", "filter", "show", "dev", bridgeName, "parent", htbRootHandle)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tc filter show: %w", err)
	}
	return parseBridgeFilters(string(output)), nil
}

func deleteBridgeFilter(bridgeName, handle string) error {
	cmd := exec.Command("tc", "filter", "del", "dev", bridgeName, "parent", htbRootHandle,
		"protocol", "all", "prio", "1", "handle", handle, "basic")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc filter del: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func deleteBridgeClass(bridgeName, fullClassID string) error {
	qdiscCmd := exec.Command("tc", "qdisc", "del", "dev", bridgeName, "parent", fullClassID)
	qdiscCmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	qdiscCmd.Run() // Best effort - may not exist

	cmd := exec.Command("tc", "class", "del", "dev", bridgeName, "classid", fullClassID)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc class del: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func minorClassID(fullClassID string) (string, bool) {
	major, minor, ok := strings.Cut(fullClassID, ":")
	if !ok || major != "1" || minor == "" {
		return "", false
	}
	return minor, true
}

func parseBridgeClasses(output string) []string {
	var classes []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "class htb 1:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			classes = append(classes, fields[2])
		}
	}
	return classes
}

func countBridgeHTBClasses(classes []string) int64 {
	var count int64
	for _, class := range classes {
		if class == htbRootClassID {
			continue
		}
		count++
	}
	return count
}

func (m *manager) bridgeHTBClassCount(ctx context.Context) (int64, error) {
	bridgeName := m.config.Network.BridgeName
	cmd := exec.CommandContext(ctx, "tc", "class", "show", "dev", bridgeName)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("tc class show: %w", err)
	}
	return countBridgeHTBClasses(parseBridgeClasses(string(output))), nil
}

func planOrphanedBridgeTC(liveTapIndexes map[int]bool, filters []bridgeFilter, classes []string) ([]bridgeFilter, []string, bool) {
	candidates, parsed := 0, 0
	for _, filter := range filters {
		if filter.handle == "" || filter.flowID == "" {
			continue
		}
		candidates++
		if filter.rtIif >= 0 {
			parsed++
		}
	}
	if candidates > 0 && parsed == 0 {
		return nil, nil, false
	}

	liveClassIDs := make(map[string]bool)
	staleFilters := make([]bridgeFilter, 0)
	for _, filter := range filters {
		if filter.handle == "" || filter.flowID == "" {
			continue
		}
		if liveTapIndexes[filter.rtIif] {
			if classID, ok := minorClassID(filter.flowID); ok {
				liveClassIDs[classID] = true
			}
			continue
		}
		staleFilters = append(staleFilters, filter)
	}

	staleClasses := make([]string, 0)
	for _, fullClassID := range classes {
		if fullClassID == htbRootClassID {
			continue
		}
		classID, ok := minorClassID(fullClassID)
		if !ok {
			continue
		}
		if !liveClassIDs[classID] {
			staleClasses = append(staleClasses, fullClassID)
		}
	}
	return staleFilters, staleClasses, true
}

// removeVMClass removes bridge tc state proven to belong to a TAP.
func (m *manager) removeVMClass(bridgeName string, tapIndex int) error {
	if tapIndex <= 0 {
		return nil
	}

	filters, err := listBridgeFilters(bridgeName)
	if err != nil {
		logger.FromContext(context.Background()).ErrorContext(context.Background(),
			"failed to list bridge filters for TAP tc cleanup",
			"bridge", bridgeName,
			"tap_ifindex", tapIndex,
			"error", err,
		)
		return nil
	}

	deletedClasses := make(map[string]struct{})
	for _, filter := range filters {
		if filter.rtIif != tapIndex {
			continue
		}
		_ = deleteBridgeFilter(bridgeName, filter.handle)
		if _, ok := deletedClasses[filter.flowID]; ok {
			continue
		}
		deletedClasses[filter.flowID] = struct{}{}
		_ = deleteBridgeClass(bridgeName, filter.flowID)
	}

	return nil
}

// deriveClassIDVal derives the numeric HTB class ID from a TAP name.
// Uses FNV-1a hash truncated to 16 bits. Returns a value in 0x0002-0xFFFF range,
// avoiding 0 (invalid) and 1 (reserved for root class 1:1).
func deriveClassIDVal(tapName string) uint16 {
	h := fnv.New32a()
	h.Write([]byte(tapName))
	val := uint16(h.Sum32() & 0xFFFF)
	if val <= 1 {
		val = 2 // 0 is invalid, 1 is root class (1:1)
	}
	return val
}

// deriveClassID derives a unique HTB class ID string from a TAP name.
func deriveClassID(tapName string) string {
	return fmt.Sprintf("%04x", deriveClassIDVal(tapName))
}

// deleteTAPDevice removes TAP device and its associated HTB class on the bridge.
func (m *manager) deleteTAPDevice(tapName string) error {
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		// TAP doesn't exist, nothing to do
		return nil
	}

	// Remove HTB class from bridge before deleting TAP
	m.removeVMClass(m.config.Network.BridgeName, link.Attrs().Index)

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete TAP device: %w", err)
	}

	return nil
}

func (m *manager) tapDeviceExists(tapName string) bool {
	_, err := netlink.LinkByName(tapName)
	return err == nil
}

// queryNetworkState queries kernel for bridge state
func (m *manager) queryNetworkState(bridgeName string) (*Network, error) {
	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return nil, ErrNotFound
	}

	// Verify it's actually a bridge
	if link.Type() != "bridge" {
		return nil, fmt.Errorf("link %s is not a bridge", bridgeName)
	}

	// Get IP addresses
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("bridge has no IP addresses")
	}

	// Use first IP as gateway
	gateway := addrs[0].IP.String()
	subnet := addrs[0].IPNet.String()

	// Bridge exists and has IP - that's sufficient
	// OperState can be OperUp, OperUnknown, etc. - all are functional for our purposes

	return &Network{
		Bridge:  bridgeName,
		Gateway: gateway,
		Subnet:  subnet,
	}, nil
}

// CleanupOrphanedTAPs removes TAP devices that aren't used by any running instance.
// preserveInstanceIDs is the authoritative set of instance IDs whose TAPs must be
// kept. Pass nil or an empty list to skip cleanup entirely (used when we couldn't
// determine an authoritative list of running instances for this host).
// minAge>0 skips TAPs whose sysfs ctime is younger than minAge. This avoids racing
// against in-flight CreateAllocation calls whose instance metadata hasn't been
// persisted yet. Pass 0 to disable the age filter (e.g. for the startup pass which
// runs before any concurrent creates can be in flight).
// Returns the number of TAPs deleted.
func (m *manager) CleanupOrphanedTAPs(ctx context.Context, preserveInstanceIDs []string, minAge time.Duration) int {
	log := logger.FromContext(ctx)

	// Skip cleanup when we don't have an authoritative running set.
	// This avoids deleting TAPs created by other concurrent hypeman processes/tests.
	if len(preserveInstanceIDs) == 0 {
		log.DebugContext(ctx, "skipping TAP cleanup (empty instance list)")
		return 0
	}

	// Build set of expected TAP names for running instances
	expectedTAPs := make(map[string]bool)
	for _, id := range preserveInstanceIDs {
		tapName := GenerateTAPName(id)
		expectedTAPs[tapName] = true
	}

	// List all network interfaces
	links, err := netlink.LinkList()
	if err != nil {
		log.WarnContext(ctx, "failed to list network links for TAP cleanup", "error", err)
		return 0
	}

	deleted := 0
	now := time.Now()
	for _, link := range links {
		name := link.Attrs().Name

		// Only consider TAP devices with our naming prefix
		if !strings.HasPrefix(name, TAPPrefix) {
			continue
		}

		// Check if this TAP is expected (belongs to a running instance)
		if expectedTAPs[name] {
			continue
		}

		// Age filter: avoid clobbering TAPs from in-flight CreateAllocation calls
		// whose instance metadata hasn't been persisted yet. sysfs ctime resets on
		// host reboot, which is fine because the startup pass runs without an age
		// filter at a time when no concurrent creates exist.
		if minAge > 0 {
			age, err := tapDeviceAge(name, now)
			if err != nil {
				log.WarnContext(ctx, "failed to stat TAP for age check, skipping", "tap", name, "error", err)
				continue
			}
			if age < minAge {
				log.DebugContext(ctx, "TAP younger than minAge, skipping", "tap", name, "age", age, "min_age", minAge)
				continue
			}
		}

		// Orphaned TAP - delete it by tc filters anchored to the TAP ifindex.
		if err := m.deleteTAPDeviceSerialized(name); err != nil {
			log.WarnContext(ctx, "failed to delete orphaned TAP", "tap", name, "error", err)
			continue
		}
		log.InfoContext(ctx, "deleted orphaned TAP device", "tap", name)
		m.recordTAPOperation(ctx, "cleanup_orphan")
		deleted++
	}

	return deleted
}

// tapDeviceAge returns how long the given netdev has existed in the current
// kernel, derived from the ctime of /sys/class/net/<name>. The clock resets on
// host reboot, but the startup CleanupOrphanedTAPs pass runs once at boot with
// no age filter, so the reset is safe.
func tapDeviceAge(name string, now time.Time) (time.Duration, error) {
	info, err := os.Stat("/sys/class/net/" + name)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type for %s", name)
	}
	ctime := time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
	return now.Sub(ctime), nil
}

// CleanupOrphanedClasses removes bridge filters and HTB classes that are no
// longer referenced by a live TAP's filter. Returns the number of tc objects
// deleted.
func (m *manager) CleanupOrphanedClasses(ctx context.Context) int {
	log := logger.FromContext(ctx)
	bridgeName := m.config.Network.BridgeName

	m.tcMu.Lock()
	defer m.tcMu.Unlock()

	cmd := exec.Command("tc", "class", "show", "dev", bridgeName)
	output, err := cmd.Output()
	if err != nil {
		log.DebugContext(ctx, "no HTB classes to clean up", "bridge", bridgeName)
		return 0
	}
	classes := parseBridgeClasses(string(output))
	if len(classes) == 0 {
		return 0
	}

	liveTapIndexes := make(map[int]bool)
	links, err := netlink.LinkList()
	if err != nil {
		log.WarnContext(ctx, "skipping orphaned tc cleanup: failed to list links", "error", err)
		return 0
	}
	for _, link := range links {
		name := link.Attrs().Name
		if strings.HasPrefix(name, TAPPrefix) {
			liveTapIndexes[link.Attrs().Index] = true
		}
	}

	filters, err := listBridgeFilters(bridgeName)
	if err != nil {
		log.WarnContext(ctx, "skipping orphaned tc cleanup: failed to list filters", "error", err)
		return 0
	}

	staleFilters, staleClasses, safe := planOrphanedBridgeTC(liveTapIndexes, filters, classes)
	if !safe {
		log.WarnContext(ctx, "skipping orphaned tc cleanup: no rt_iif matches parsed from tc filter output",
			"bridge", bridgeName, "filters", len(filters))
		return 0
	}

	deleted := 0
	for _, filter := range staleFilters {
		log.WarnContext(ctx, "cleaning up orphaned tc filter",
			"handle", filter.handle, "flowid", filter.flowID, "rt_iif", filter.rtIif, "bridge", bridgeName)
		if err := deleteBridgeFilter(bridgeName, filter.handle); err != nil {
			log.WarnContext(ctx, "failed to delete orphaned tc filter",
				"handle", filter.handle, "error", err)
			continue
		}
		deleted++
	}

	for _, fullClassID := range staleClasses {
		log.WarnContext(ctx, "cleaning up orphaned HTB class", "class", fullClassID, "bridge", bridgeName)
		if err := deleteBridgeClass(bridgeName, fullClassID); err != nil {
			log.WarnContext(ctx, "failed to delete orphaned class", "class", fullClassID, "error", err)
			continue
		}
		deleted++
	}

	return deleted
}
