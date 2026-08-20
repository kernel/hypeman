//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
	"unsafe"

	pb "github.com/kernel/hypeman/lib/guest"
	"golang.org/x/sys/windows"
)

const (
	addressFamilyIPv4       = 2
	gaaFlagIncludePrefixes  = 0x10
	dnsSettingsVersion1     = 1
	dnsSettingNameServer    = 0x2
	windowsErrorBufferLarge = syscall.Errno(111)
	windowsErrorNotFound    = syscall.Errno(1168)
)

var (
	errWindowsAdapterNotFound    = errors.New("Windows network adapter not found")
	ipHelperDLL                  = windows.NewLazySystemDLL("iphlpapi.dll")
	initializeUnicastAddressProc = ipHelperDLL.NewProc("InitializeUnicastIpAddressEntry")
	createUnicastAddressProc     = ipHelperDLL.NewProc("CreateUnicastIpAddressEntry")
	deleteUnicastAddressProc     = ipHelperDLL.NewProc("DeleteUnicastIpAddressEntry")
	initializeForwardEntryProc   = ipHelperDLL.NewProc("InitializeIpForwardEntry")
	createForwardEntryProc       = ipHelperDLL.NewProc("CreateIpForwardEntry2")
	deleteForwardEntryProc       = ipHelperDLL.NewProc("DeleteIpForwardEntry2")
	setInterfaceDNSSettingsProc  = ipHelperDLL.NewProc("SetInterfaceDnsSettings")
)

type dnsInterfaceSettings struct {
	Version             uint32
	Flags               uint64
	Domain              *uint16
	NameServer          *uint16
	SearchList          *uint16
	RegistrationEnabled uint32
	RegisterAdapterName uint32
	EnableLLMNR         uint32
	QueryAdapterName    uint32
	ProfileNameServer   *uint16
}

func (s *guestServer) ReconfigureNetwork(ctx context.Context, req *pb.ReconfigureNetworkRequest) (*pb.ReconfigureNetworkResponse, error) {
	mac, err := net.ParseMAC(req.Mac)
	if err != nil {
		return nil, fmt.Errorf("parse mac %q: %w", req.Mac, err)
	}
	ipv4 := net.ParseIP(req.Ipv4).To4()
	if ipv4 == nil {
		return nil, fmt.Errorf("parse ipv4 %q", req.Ipv4)
	}
	gateway := net.ParseIP(req.Gateway).To4()
	if gateway == nil {
		return nil, fmt.Errorf("parse gateway %q", req.Gateway)
	}
	if req.Prefix > 32 {
		return nil, fmt.Errorf("invalid ipv4 prefix %d", req.Prefix)
	}
	for _, server := range req.DnsServers {
		if net.ParseIP(server).To4() == nil {
			return nil, fmt.Errorf("parse DNS server %q", server)
		}
	}

	adapter, err := waitForWindowsAdapter(ctx, mac, req.InterfaceName)
	if err != nil {
		return nil, err
	}
	if err := configureWindowsAddresses(adapter, ipv4, uint8(req.Prefix), gateway); err != nil {
		return nil, err
	}
	if len(req.DnsServers) > 0 {
		if err := configureWindowsDNS(adapter.NetworkGUID, req.DnsServers); err != nil {
			return nil, err
		}
	}
	return &pb.ReconfigureNetworkResponse{}, nil
}

type windowsAdapterInfo struct {
	Luid        uint64
	Index       uint32
	NetworkGUID windows.GUID
	IPv4        []net.IP
}

func waitForWindowsAdapter(ctx context.Context, mac net.HardwareAddr, name string) (*windowsAdapterInfo, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		adapter, err := findWindowsAdapter(mac, name)
		if err == nil {
			return adapter, nil
		}
		if !errors.Is(err, errWindowsAdapterNotFound) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for Windows network adapter: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func findWindowsAdapter(mac net.HardwareAddr, name string) (*windowsAdapterInfo, error) {
	size := uint32(15 * 1024)
	for {
		buffer := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
		err := windows.GetAdaptersAddresses(addressFamilyIPv4, gaaFlagIncludePrefixes, 0, first, &size)
		if err == windowsErrorBufferLarge {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list Windows network adapters: %w", err)
		}
		for adapter := first; adapter != nil; adapter = adapter.Next {
			physical := net.HardwareAddr(adapter.PhysicalAddress[:adapter.PhysicalAddressLength])
			friendlyName := windows.UTF16PtrToString(adapter.FriendlyName)
			if strings.EqualFold(physical.String(), mac.String()) && (name == "" || strings.EqualFold(name, friendlyName)) {
				result := &windowsAdapterInfo{Luid: adapter.Luid, Index: adapter.IfIndex, NetworkGUID: adapter.NetworkGuid}
				for address := adapter.FirstUnicastAddress; address != nil; address = address.Next {
					if ipv4 := address.Address.IP().To4(); ipv4 != nil {
						result.IPv4 = append(result.IPv4, append(net.IP(nil), ipv4...))
					}
				}
				return result, nil
			}
		}
		return nil, fmt.Errorf("%w: MAC %s", errWindowsAdapterNotFound, mac)
	}
}

func configureWindowsAddresses(adapter *windowsAdapterInfo, ipv4 net.IP, prefix uint8, gateway net.IP) error {
	for _, address := range adapter.IPv4 {
		var row windows.MibUnicastIpAddressRow
		initializeUnicastAddressProc.Call(uintptr(unsafe.Pointer(&row)))
		row.InterfaceLuid = adapter.Luid
		row.InterfaceIndex = adapter.Index
		copyRawIPv4(unsafe.Pointer(&row.Address), address)
		if code, _, _ := deleteUnicastAddressProc.Call(uintptr(unsafe.Pointer(&row))); code != 0 && syscall.Errno(code) != windowsErrorNotFound {
			return fmt.Errorf("delete Windows IPv4 address: %w", syscall.Errno(code))
		}
	}

	var routes *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(addressFamilyIPv4, &routes); err != nil {
		return fmt.Errorf("list Windows IPv4 routes: %w", err)
	}
	if routes != nil {
		defer windows.FreeMibTable(unsafe.Pointer(routes))
		for _, route := range routes.Rows() {
			destination := (*windows.RawSockaddrInet4)(unsafe.Pointer(&route.DestinationPrefix.Prefix))
			if route.InterfaceLuid == adapter.Luid && route.DestinationPrefix.PrefixLength == 0 && destination.Addr == [4]byte{} {
				row := route
				if code, _, _ := deleteForwardEntryProc.Call(uintptr(unsafe.Pointer(&row))); code != 0 && syscall.Errno(code) != windowsErrorNotFound {
					return fmt.Errorf("delete Windows default route: %w", syscall.Errno(code))
				}
			}
		}
	}

	var addressRow windows.MibUnicastIpAddressRow
	initializeUnicastAddressProc.Call(uintptr(unsafe.Pointer(&addressRow)))
	addressRow.InterfaceLuid = adapter.Luid
	addressRow.InterfaceIndex = adapter.Index
	addressRow.OnLinkPrefixLength = prefix
	copyRawIPv4(unsafe.Pointer(&addressRow.Address), ipv4)
	if code, _, _ := createUnicastAddressProc.Call(uintptr(unsafe.Pointer(&addressRow))); code != 0 {
		return fmt.Errorf("create Windows IPv4 address: %w", syscall.Errno(code))
	}

	var route windows.MibIpForwardRow2
	initializeForwardEntryProc.Call(uintptr(unsafe.Pointer(&route)))
	route.InterfaceLuid = adapter.Luid
	route.InterfaceIndex = adapter.Index
	route.DestinationPrefix.PrefixLength = 0
	copyRawIPv4(unsafe.Pointer(&route.DestinationPrefix.Prefix), net.IPv4zero)
	copyRawIPv4(unsafe.Pointer(&route.NextHop), gateway)
	route.Protocol = windows.MIB_IPPROTO_NETMGMT
	route.Origin = windows.NlroManual
	if code, _, _ := createForwardEntryProc.Call(uintptr(unsafe.Pointer(&route))); code != 0 {
		return fmt.Errorf("create Windows default route: %w", syscall.Errno(code))
	}
	return nil
}

func configureWindowsDNS(interfaceGUID windows.GUID, servers []string) error {
	nameServers, err := windows.UTF16PtrFromString(strings.Join(servers, ","))
	if err != nil {
		return err
	}
	settings := dnsInterfaceSettings{Version: dnsSettingsVersion1, Flags: dnsSettingNameServer, NameServer: nameServers}
	code, _, _ := setInterfaceDNSSettingsProc.Call(
		uintptr(unsafe.Pointer(&interfaceGUID)),
		uintptr(unsafe.Pointer(&settings)),
	)
	if code != 0 {
		return fmt.Errorf("set Windows DNS servers: %w", syscall.Errno(code))
	}
	return nil
}

func copyRawIPv4(destination unsafe.Pointer, ip net.IP) {
	address := (*windows.RawSockaddrInet4)(destination)
	address.Family = addressFamilyIPv4
	copy(address.Addr[:], ip.To4())
}
