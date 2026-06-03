package main

import (
	"context"
	"fmt"
	"net"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/system/netoffload"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func (s *guestServer) ReconfigureNetwork(_ context.Context, req *pb.ReconfigureNetworkRequest) (*pb.ReconfigureNetworkResponse, error) {
	iface := req.InterfaceName
	if iface == "" {
		iface = "eth0"
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("find interface %s: %w", iface, err)
	}

	mac, err := net.ParseMAC(req.Mac)
	if err != nil {
		return nil, fmt.Errorf("parse mac %q: %w", req.Mac, err)
	}
	ip := net.ParseIP(req.Ipv4).To4()
	if ip == nil {
		return nil, fmt.Errorf("parse ipv4 %q", req.Ipv4)
	}
	if req.Prefix > 32 {
		return nil, fmt.Errorf("invalid ipv4 prefix %d", req.Prefix)
	}
	gateway := net.ParseIP(req.Gateway).To4()
	if gateway == nil {
		return nil, fmt.Errorf("parse gateway %q", req.Gateway)
	}

	if err := netlink.LinkSetDown(link); err != nil {
		return nil, fmt.Errorf("set %s down: %w", iface, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
		return nil, fmt.Errorf("set %s mac: %w", iface, err)
	}
	if err := flushIPv4Addrs(link); err != nil {
		return nil, err
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(int(req.Prefix), 32)}}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return nil, fmt.Errorf("add ipv4 address: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("set %s up: %w", iface, err)
	}
	_ = netoffload.DisableTXChecksum(iface)
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        gateway,
	}); err != nil {
		return nil, fmt.Errorf("replace default route: %w", err)
	}
	_ = flushNeighbors(link)

	return &pb.ReconfigureNetworkResponse{}, nil
}

func flushIPv4Addrs(link netlink.Link) error {
	addrs, err := netlink.AddrList(link, unix.AF_INET)
	if err != nil {
		return fmt.Errorf("list ipv4 addresses: %w", err)
	}
	for _, addr := range addrs {
		if err := netlink.AddrDel(link, &addr); err != nil {
			return fmt.Errorf("delete ipv4 address %s: %w", addr.String(), err)
		}
	}
	return nil
}

func flushNeighbors(link netlink.Link) error {
	neighbors, err := netlink.NeighList(link.Attrs().Index, unix.AF_INET)
	if err != nil {
		return fmt.Errorf("list neighbors: %w", err)
	}
	for _, neighbor := range neighbors {
		if err := netlink.NeighDel(&neighbor); err != nil {
			return fmt.Errorf("delete neighbor %s: %w", neighbor.String(), err)
		}
	}
	return nil
}
