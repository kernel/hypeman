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

	// Get uplink interface
	uplink := "eth0"
	if m.config.UplinkInterface != "" {
		uplink = m.config.UplinkInterface
	}

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

// ensureNATRule ensures the MASQUERADE rule exists
func (m *manager) ensureNATRule(subnet, uplink string) (string, error) {
	// Check if rule exists (with or without comment)
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

	// Delete old rule without comment (migration from old code)
	delOld := exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", subnet, "-o", uplink, "-j", "MASQUERADE")
	delOld.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	delOld.Run() // ignore error

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

// ensureForwardRule ensures a FORWARD rule exists at the correct position (top of chain)
func (m *manager) ensureForwardRule(inIface, outIface, ctstate, comment string, position int) (string, error) {
	// Check if rule with comment exists at correct position
	if m.isForwardRuleAtPosition(inIface, outIface, ctstate, comment, position) {
		return "existing", nil
	}

	// Delete old rule without comment (migration from old code)
	delOld := exec.Command("iptables", "-D", "FORWARD",
		"-i", inIface, "-o", outIface,
		"-m", "conntrack", "--ctstate", ctstate,
		"-j", "ACCEPT")
	delOld.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	delOld.Run() // ignore error

	// Delete old rule with comment if exists (might be at wrong position)
	delWithComment := exec.Command("iptables", "-D", "FORWARD",
		"-i", inIface, "-o", outIface,
		"-m", "conntrack", "--ctstate", ctstate,
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT")
	delWithComment.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	delWithComment.Run() // ignore error

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

// isForwardRuleAtPosition checks if our rule exists at the expected position
func (m *manager) isForwardRuleAtPosition(inIface, outIface, ctstate, comment string, position int) bool {
	// List FORWARD chain with line numbers
	cmd := exec.Command("iptables", "-L", "FORWARD", "--line-numbers", "-n")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Look for our comment at the expected position
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Line format: "1    ACCEPT  ... /* hypeman-fwd-out */"
		if strings.Contains(line, comment) {
			// Check if it starts with the expected position number
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == fmt.Sprintf("%d", position) {
				return true
			}
		}
	}
	return false
}

// createTAPDevice creates TAP device and attaches to bridge
func (m *manager) createTAPDevice(tapName, bridgeName string, isolated bool) error {
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

	return nil
}

// deleteTAPDevice removes TAP device
func (m *manager) deleteTAPDevice(tapName string) error {
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
// Returns the number of TAPs deleted.
func (m *manager) CleanupOrphanedTAPs(ctx context.Context, runningInstanceIDs []string) int {
	log := logger.FromContext(ctx)

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

