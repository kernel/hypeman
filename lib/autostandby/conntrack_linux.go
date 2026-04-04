//go:build linux

package autostandby

import (
	"context"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// ConntrackSource reads current IPv4 TCP conntrack entries from the host.
type ConntrackSource struct {
	listFlows func(table netlink.ConntrackTableType, family netlink.InetFamily) ([]*netlink.ConntrackFlow, error)
}

// NewConntrackSource creates a conntrack-backed connection source.
func NewConntrackSource() *ConntrackSource {
	return &ConntrackSource{
		listFlows: netlink.ConntrackTableList,
	}
}

// ListConnections returns normalized TCP flows from the host conntrack table.
func (s *ConntrackSource) ListConnections(context.Context) ([]Connection, error) {
	flows, err := s.listFlows(netlink.ConntrackTable, netlink.InetFamily(unix.AF_INET))
	if err != nil {
		return nil, err
	}
	return connectionsFromFlows(flows), nil
}

func connectionsFromFlows(flows []*netlink.ConntrackFlow) []Connection {
	result := make([]Connection, 0, len(flows))
	for _, flow := range flows {
		conn, ok := connectionFromFlow(flow)
		if !ok {
			continue
		}
		result = append(result, conn)
	}
	return result
}

func connectionFromFlow(flow *netlink.ConntrackFlow) (Connection, bool) {
	if flow == nil || flow.Forward.Protocol != unix.IPPROTO_TCP {
		return Connection{}, false
	}

	tcpInfo, ok := flow.ProtoInfo.(*netlink.ProtoInfoTCP)
	if !ok || tcpInfo == nil {
		return Connection{}, false
	}

	srcIP, ok := addrFromIP(flow.Forward.SrcIP)
	if !ok {
		return Connection{}, false
	}
	dstIP, ok := addrFromIP(flow.Forward.DstIP)
	if !ok {
		return Connection{}, false
	}

	return Connection{
		OriginalSourceIP:        srcIP,
		OriginalDestinationIP:   dstIP,
		OriginalDestinationPort: flow.Forward.DstPort,
		TCPState:                TCPState(tcpInfo.State),
	}, true
}

func addrFromIP(ip []byte) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
