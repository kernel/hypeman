package network

import (
	"fmt"
	"net"
	"os/exec"

	"github.com/vishvananda/netlink"
)

// createBridge creates a bridge interface using netlink
func (m *manager) createBridge(name, gateway, subnet string) error {
	// 1. Parse subnet to get network and prefix length
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("parse subnet: %w", err)
	}

	// 2. Check if bridge already exists
	existing, err := netlink.LinkByName(name)
	if err == nil {
		// Bridge exists, verify it's up
		if err := netlink.LinkSetUp(existing); err != nil {
			return fmt.Errorf("set bridge up: %w", err)
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

	// 6. Setup iptables rules
	if err := m.setupIPTablesRules(subnet, name); err != nil {
		return fmt.Errorf("setup iptables: %w", err)
	}

	return nil
}

// setupIPTablesRules sets up NAT and forwarding rules
func (m *manager) setupIPTablesRules(subnet, bridgeName string) error {
	// Enable IP forwarding
	cmd := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}

	// Get uplink interface (usually eth0, but could be different)
	// For now, we'll use a common default
	// TODO @sjmiller609 review
	uplink := "eth0"
	if m.config.UplinkInterface != "" {
		uplink = m.config.UplinkInterface
	}

	// Add MASQUERADE rule if not exists
	// TODO @sjmiller609 review security here and down in this function
	checkCmd := exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", subnet, "-o", uplink, "-j", "MASQUERADE")
	if err := checkCmd.Run(); err != nil {
		// Rule doesn't exist, add it
		addCmd := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING",
			"-s", subnet, "-o", uplink, "-j", "MASQUERADE")
		if err := addCmd.Run(); err != nil {
			return fmt.Errorf("add masquerade rule: %w", err)
		}
	}

	// Add FORWARD rules for outbound connections
	checkForwardOut := exec.Command("iptables", "-C", "FORWARD",
		"-i", bridgeName, "-o", uplink,
		"-m", "conntrack", "--ctstate", "NEW,ESTABLISHED,RELATED",
		"-j", "ACCEPT")
	if err := checkForwardOut.Run(); err != nil {
		addForwardOut := exec.Command("iptables", "-A", "FORWARD",
			"-i", bridgeName, "-o", uplink,
			"-m", "conntrack", "--ctstate", "NEW,ESTABLISHED,RELATED",
			"-j", "ACCEPT")
		if err := addForwardOut.Run(); err != nil {
			return fmt.Errorf("add forward outbound rule: %w", err)
		}
	}

	// Add FORWARD rules for inbound responses
	checkForwardIn := exec.Command("iptables", "-C", "FORWARD",
		"-i", uplink, "-o", bridgeName,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "ACCEPT")
	if err := checkForwardIn.Run(); err != nil {
		addForwardIn := exec.Command("iptables", "-A", "FORWARD",
			"-i", uplink, "-o", bridgeName,
			"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
			"-j", "ACCEPT")
		if err := addForwardIn.Run(); err != nil {
			return fmt.Errorf("add forward inbound rule: %w", err)
		}
	}

	return nil
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

	// 2. Create TAP device
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: tapName,
		},
		Mode: netlink.TUNTAP_MODE_TAP,
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

	// 5. Set isolation mode
	if isolated {
		// Use shell command for bridge_slave isolated flag
		// netlink library doesn't expose this flag yet
		cmd := exec.Command("ip", "link", "set", tapName, "type", "bridge_slave", "isolated", "on")
		if err := cmd.Run(); err != nil {
			// Log warning but don't fail - isolation is a security feature but not critical
			// for basic functionality
			return fmt.Errorf("set TAP isolation: %w", err)
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

	// Check if link is up
	if link.Attrs().OperState != netlink.OperUp {
		return nil, fmt.Errorf("bridge is not up")
	}

	return &Network{
		Bridge:  bridgeName,
		Gateway: gateway,
		Subnet:  subnet,
	}, nil
}

