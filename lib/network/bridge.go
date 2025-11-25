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
		// Bridge exists, verify it's up
		if err := netlink.LinkSetUp(existing); err != nil {
			return fmt.Errorf("set bridge up: %w", err)
		}
		log.InfoContext(ctx, "bridge ready", "bridge", name, "status", "existing")
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

	// Add MASQUERADE rule if not exists
	checkCmd := exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", subnet, "-o", uplink, "-j", "MASQUERADE")
	checkCmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	masqStatus := "existing"
	if err := checkCmd.Run(); err != nil {
		// Rule doesn't exist, add it
		addCmd := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING",
			"-s", subnet, "-o", uplink, "-j", "MASQUERADE")
		addCmd.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		if err := addCmd.Run(); err != nil {
			return fmt.Errorf("add masquerade rule: %w", err)
		}
		masqStatus = "added"
	}
	log.InfoContext(ctx, "iptables NAT ready", "subnet", subnet, "uplink", uplink, "status", masqStatus)

	// Allow bridge-initiated flows to traverse the host uplink
	checkForwardOut := exec.Command("iptables", "-C", "FORWARD",
		"-i", bridgeName, "-o", uplink,
		"-m", "conntrack", "--ctstate", "NEW,ESTABLISHED,RELATED",
		"-j", "ACCEPT")
	checkForwardOut.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	fwdOutStatus := "existing"
	if err := checkForwardOut.Run(); err != nil {
		addForwardOut := exec.Command("iptables", "-A", "FORWARD",
			"-i", bridgeName, "-o", uplink,
			"-m", "conntrack", "--ctstate", "NEW,ESTABLISHED,RELATED",
			"-j", "ACCEPT")
		addForwardOut.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		if err := addForwardOut.Run(); err != nil {
			return fmt.Errorf("add forward outbound rule: %w", err)
		}
		fwdOutStatus = "added"
	}

	// Permit return traffic from the uplink back to the bridge
	checkForwardIn := exec.Command("iptables", "-C", "FORWARD",
		"-i", uplink, "-o", bridgeName,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "ACCEPT")
	checkForwardIn.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	fwdInStatus := "existing"
	if err := checkForwardIn.Run(); err != nil {
		addForwardIn := exec.Command("iptables", "-A", "FORWARD",
			"-i", uplink, "-o", bridgeName,
			"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
			"-j", "ACCEPT")
		addForwardIn.SysProcAttr = &syscall.SysProcAttr{
			AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
		}
		if err := addForwardIn.Run(); err != nil {
			return fmt.Errorf("add forward inbound rule: %w", err)
		}
		fwdInStatus = "added"
	}

	log.InfoContext(ctx, "iptables FORWARD ready", "outbound", fwdOutStatus, "inbound", fwdInStatus)

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

