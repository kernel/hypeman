package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// DeriveGateway returns the first usable IP in a subnet (used as gateway).
// e.g., 10.100.0.0/16 -> 10.100.0.1
func DeriveGateway(cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse CIDR: %w", err)
	}

	// Gateway is network address + 1
	gateway := make(net.IP, len(ipNet.IP))
	copy(gateway, ipNet.IP)
	gateway[len(gateway)-1]++ // Increment last octet

	return gateway.String(), nil
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
			if ifaceName == m.config.BridgeName {
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
		addrs, err := netlink.AddrList(existing, netlink.FAMILY_V4)
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
	commentNAT    = "hypeman-nat"
	commentFwdOut = "hypeman-fwd-out"
	commentFwdIn  = "hypeman-fwd-in"
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
	if m.config.UplinkInterface != "" {
		return m.config.UplinkInterface, nil
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

	// Add MASQUERADE rule if not exists (position doesn't matter in POSTROUTING)
	masqStatus, err := m.ensureNATRule(subnet, uplink)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "iptables NAT ready", "subnet", subnet, "uplink", uplink, "status", masqStatus)

	// FORWARD rules must be at top of chain (before Docker's DOCKER-USER/DOCKER-FORWARD)
	// We insert at position 1 and 2 to ensure they're evaluated first
	fwdOutStatus, err := m.ensureForwardRule(bridgeName, uplink, "NEW,ESTABLISHED,RELATED", commentFwdOut, 1)
	if err != nil {
		return fmt.Errorf("setup forward outbound: %w", err)
	}

	fwdInStatus, err := m.ensureForwardRule(uplink, bridgeName, "ESTABLISHED,RELATED", commentFwdIn, 2)
	if err != nil {
		return fmt.Errorf("setup forward inbound: %w", err)
	}

	log.InfoContext(ctx, "iptables FORWARD ready", "outbound", fwdOutStatus, "inbound", fwdInStatus)

	return nil
}

// ensureNATRule ensures the MASQUERADE rule exists with correct uplink
func (m *manager) ensureNATRule(subnet, uplink string) (string, error) {
	// Check if rule exists with correct subnet and uplink
	checkCmd := exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", subnet, "-o", uplink,
		"-m", "comment", "--comment", commentNAT,
		"-j", "MASQUERADE")
	checkCmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if checkCmd.Run() == nil {
		return "existing", nil
	}

	// Delete any existing rule with our comment (handles uplink changes)
	m.deleteNATRuleByComment(commentNAT)

	// Add rule with comment
	addCmd := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "-o", uplink,
		"-m", "comment", "--comment", commentNAT,
		"-j", "MASQUERADE")
	addCmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if err := addCmd.Run(); err != nil {
		return "", fmt.Errorf("add masquerade rule: %w", err)
	}
	return "added", nil
}

// deleteNATRuleByComment deletes any NAT POSTROUTING rule containing our comment
func (m *manager) deleteNATRuleByComment(comment string) {
	// List NAT POSTROUTING rules
	cmd := exec.Command("iptables", "-t", "nat", "-L", "POSTROUTING", "--line-numbers", "-n")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
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
		delCmd := exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", ruleNums[i])
		delCmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
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
	addCmd := exec.Command("iptables", "-I", "FORWARD", fmt.Sprintf("%d", position),
		"-i", inIface, "-o", outIface,
		"-m", "conntrack", "--ctstate", ctstate,
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT")
	addCmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if err := addCmd.Run(); err != nil {
		return "", fmt.Errorf("insert forward rule: %w", err)
	}
	return "added", nil
}

// isForwardRuleCorrect checks if our rule exists at the expected position with correct interfaces
func (m *manager) isForwardRuleCorrect(inIface, outIface, comment string, position int) bool {
	// List FORWARD chain with line numbers
	cmd := exec.Command("iptables", "-L", "FORWARD", "--line-numbers", "-n", "-v")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
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
	cmd := exec.Command("iptables", "-L", "FORWARD", "--line-numbers", "-n")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
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
		delCmd := exec.Command("iptables", "-D", "FORWARD", ruleNums[i])
		delCmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		delCmd.Run() // ignore error
	}
}

// createTAPDevice creates TAP device and attaches to bridge.
// downloadBps: rate limit for download (external→VM), applied as TBF on TAP egress
// uploadBps/uploadCeilBps: rate limit for upload (VM→external), applied as HTB class on bridge
func (m *manager) createTAPDevice(tapName, bridgeName string, isolated bool, downloadBps, uploadBps, uploadCeilBps int64) error {
	// 1. Check if TAP already exists
	if _, err := netlink.LinkByName(tapName); err == nil {
		// TAP already exists, delete it first
		if err := m.deleteTAPDevice(tapName); err != nil {
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

	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("create TAP device: %w", err)
	}

	// 3. Set TAP up
	tapLink, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("get TAP link: %w", err)
	}

	if err := netlink.LinkSetUp(tapLink); err != nil {
		return fmt.Errorf("set TAP up: %w", err)
	}

	// 4. Attach TAP to bridge
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("get bridge: %w", err)
	}

	if err := netlink.LinkSetMaster(tapLink, bridge); err != nil {
		return fmt.Errorf("attach TAP to bridge: %w", err)
	}

	// 5. Enable port isolation so isolated TAPs can't directly talk to each other (requires kernel support and capabilities)
	if isolated {
		// Use shell command for bridge_slave isolated flag
		// netlink library doesn't expose this flag yet
		cmd := exec.Command("ip", "link", "set", tapName, "type", "bridge_slave", "isolated", "on")
		// Enable ambient capabilities so child process inherits CAP_NET_ADMIN
		cmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("set isolation mode: %w (output: %s)", err, string(output))
		}
	}

	// 6. Apply download rate limiting (TBF on TAP egress)
	if downloadBps > 0 {
		if err := m.applyDownloadRateLimit(tapName, downloadBps); err != nil {
			return fmt.Errorf("apply download rate limit: %w", err)
		}
	}

	// 7. Apply upload rate limiting (HTB class on bridge)
	if uploadBps > 0 {
		if err := m.addVMClass(bridgeName, tapName, uploadBps, uploadCeilBps); err != nil {
			return fmt.Errorf("apply upload rate limit: %w", err)
		}
	}

	return nil
}

// applyDownloadRateLimit applies download (external→VM) rate limiting using TBF on TAP egress.
func (m *manager) applyDownloadRateLimit(tapName string, rateLimitBps int64) error {
	rateStr := formatTcRate(rateLimitBps)

	// Use Token Bucket Filter (tbf) for download shaping
	// burst: typically rate/250 (for HZ=250) or at least 1540 bytes for MTU
	// latency: max time a packet can wait in queue
	burstBytes := rateLimitBps / 250
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
	output, err := cmd.CombinedOutput()
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
func (m *manager) addVMClass(bridgeName, tapName string, rateBps, ceilBps int64) error {
	if rateBps <= 0 {
		return nil // No rate limiting configured
	}

	// Use first 4 hex chars of TAP name suffix as class ID (e.g., "hype-a1b2c3d4" → "a1b2")
	// This ensures unique, stable class IDs per VM
	classID := deriveClassID(tapName)
	fullClassID := fmt.Sprintf("1:%s", classID)

	rateStr := formatTcRate(rateBps)
	if ceilBps <= 0 {
		ceilBps = rateBps
	}
	ceilStr := formatTcRate(ceilBps)

	// 1. Add HTB class for this VM
	cmd := exec.Command("tc", "class", "add", "dev", bridgeName, "parent", htbRootClassID,
		"classid", fullClassID, "htb", "rate", rateStr, "ceil", ceilStr, "prio", "1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc class add vm: %w (output: %s)", err, string(output))
	}

	// 2. Add fq_codel to this class for better latency under load
	cmd = exec.Command("tc", "qdisc", "add", "dev", bridgeName, "parent", fullClassID, "fq_codel")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	// Ignore errors - fq_codel may not be available
	cmd.Run()

	// 3. Add filter to classify traffic from this TAP to this class
	// Use basic match on incoming interface (rt_iif)
	tapLink, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("get TAP link for filter: %w", err)
	}
	tapIndex := tapLink.Attrs().Index

	cmd = exec.Command("tc", "filter", "add", "dev", bridgeName, "parent", htbRootHandle,
		"protocol", "all", "prio", "1", "basic",
		"match", fmt.Sprintf("meta(rt_iif eq %d)", tapIndex),
		"flowid", fullClassID)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc filter add: %w (output: %s)", err, string(output))
	}

	return nil
}

// removeVMClass removes the HTB class for a VM from the bridge.
func (m *manager) removeVMClass(bridgeName, tapName string) error {
	classID := deriveClassID(tapName)
	fullClassID := fmt.Sprintf("1:%s", classID)

	// Delete filter first (by matching flowid)
	// List filters and delete matching ones
	listCmd := exec.Command("tc", "filter", "show", "dev", bridgeName, "parent", htbRootHandle)
	listCmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	output, _ := listCmd.Output()

	// Parse filter output to find handle for this flowid
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, fullClassID) && strings.Contains(line, "filter") {
			// Extract filter handle (e.g., "filter parent 1: protocol all pref 1 basic chain 0 handle 0x1")
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "handle" && i+1 < len(fields) {
					handle := fields[i+1]
					delCmd := exec.Command("tc", "filter", "del", "dev", bridgeName, "parent", htbRootHandle,
						"protocol", "all", "prio", "1", "handle", handle, "basic")
					delCmd.SysProcAttr = &syscall.SysProcAttr{
						AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
					}
					delCmd.Run() // Best effort
					break
				}
			}
		}
	}

	// Delete the class
	cmd := exec.Command("tc", "class", "del", "dev", bridgeName, "classid", fullClassID)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	// Ignore errors - class may not exist
	cmd.Run()

	return nil
}

// deriveClassID derives a unique HTB class ID from a TAP name.
// Uses first 4 hex characters after the prefix (e.g., "hype-a1b2c3d4" → "a1b2").
func deriveClassID(tapName string) string {
	// TAP names are "hype-{8chars}", extract chars after prefix
	suffix := strings.TrimPrefix(tapName, TAPPrefix)
	if len(suffix) >= 4 {
		return suffix[:4]
	}
	// Fallback: use full suffix padded
	return fmt.Sprintf("%04s", suffix)
}

// formatTcRate formats bytes per second as a tc rate string.
func formatTcRate(bytesPerSec int64) string {
	bitsPerSec := bytesPerSec * 8
	switch {
	case bitsPerSec >= 1000000000:
		return fmt.Sprintf("%dgbit", bitsPerSec/1000000000)
	case bitsPerSec >= 1000000:
		return fmt.Sprintf("%dmbit", bitsPerSec/1000000)
	case bitsPerSec >= 1000:
		return fmt.Sprintf("%dkbit", bitsPerSec/1000)
	default:
		return fmt.Sprintf("%dbit", bitsPerSec)
	}
}

// deleteTAPDevice removes TAP device and its associated HTB class on the bridge.
func (m *manager) deleteTAPDevice(tapName string) error {
	// Remove HTB class from bridge before deleting TAP
	m.removeVMClass(m.config.BridgeName, tapName)

	link, err := netlink.LinkByName(tapName)
	if err != nil {
		// TAP doesn't exist, nothing to do
		return nil
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete TAP device: %w", err)
	}

	return nil
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
// runningInstanceIDs is a list of instance IDs that currently have a running VMM.
// Pass nil to skip cleanup entirely (used when we couldn't determine running instances).
// Returns the number of TAPs deleted.
func (m *manager) CleanupOrphanedTAPs(ctx context.Context, runningInstanceIDs []string) int {
	log := logger.FromContext(ctx)

	// If nil, skip cleanup entirely to avoid accidentally deleting TAPs for running VMs
	if runningInstanceIDs == nil {
		log.DebugContext(ctx, "skipping TAP cleanup (nil instance list)")
		return 0
	}

	// Build set of expected TAP names for running instances
	expectedTAPs := make(map[string]bool)
	for _, id := range runningInstanceIDs {
		tapName := generateTAPName(id)
		expectedTAPs[tapName] = true
	}

	// List all network interfaces
	links, err := netlink.LinkList()
	if err != nil {
		log.WarnContext(ctx, "failed to list network links for TAP cleanup", "error", err)
		return 0
	}

	deleted := 0
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

		// Orphaned TAP - delete it
		if err := m.deleteTAPDevice(name); err != nil {
			log.WarnContext(ctx, "failed to delete orphaned TAP", "tap", name, "error", err)
			continue
		}
		log.InfoContext(ctx, "deleted orphaned TAP device", "tap", name)
		deleted++
	}

	return deleted
}
