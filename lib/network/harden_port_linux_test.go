//go:build linux

package network

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const testEtherType = 0x88b5

// TestHardenIsolatedPortStopsUnicastLeak builds a bridge in a throwaway network
// namespace with two hardened guest ports and one port left at kernel defaults,
// then transmits frames through the bridge the way the host stack does.
func TestHardenIsolatedPortStopsUnicastLeak(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Skipping test that requires root")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer orig.Close()
	ns, err := netns.New()
	require.NoError(t, err)
	defer ns.Close()
	defer netns.Set(orig)

	require.NoError(t, netlink.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}))
	br, err := netlink.LinkByName("br0")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(br))

	guestA := addBridgePort(t, br, "a", false)
	guestB := addBridgePort(t, br, "b", false)
	control := addBridgePort(t, br, "c", true)

	waitUp(t, "br0", "port-a", "port-b", "port-c", "guest-a", "guest-b", "guest-c")
	recvA := openPacketSocket(t, guestA)
	recvB := openPacketSocket(t, guestB)
	recvControl := openPacketSocket(t, control)
	send := openPacketSocket(t, br)

	macA := guestA.Attrs().HardwareAddr
	unknown := net.HardwareAddr{0x02, 0x00, 0x00, 0xde, 0xad, 0x01}

	// Delivery to a pinned guest must not depend on flooding.
	sendFrame(t, send, br, macA, []byte("to-a"))
	assert.True(t, receivedFrame(recvA, []byte("to-a")), "pinned guest did not receive its frame")
	assert.False(t, receivedFrame(recvB, []byte("to-a")), "frame for guest A reached guest B")

	// Unknown unicast reaches a default port (the leak) but not hardened ones.
	sendFrame(t, send, br, unknown, []byte("unknown"))
	assert.True(t, receivedFrame(recvControl, []byte("unknown")), "control port should see flooded unknown unicast")
	assert.False(t, receivedFrame(recvA, []byte("unknown")), "unknown unicast flooded to hardened guest A")
	assert.False(t, receivedFrame(recvB, []byte("unknown")), "unknown unicast flooded to hardened guest B")

	// Guest B spoofing guest A's MAC must not move A's FDB entry.
	sendFrame(t, recvB, guestB, net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, []byte("spoof"), macA)
	sendFrame(t, send, br, macA, []byte("after-spoof"))
	assert.True(t, receivedFrame(recvA, []byte("after-spoof")), "guest A lost delivery after spoof")
	assert.False(t, receivedFrame(recvB, []byte("after-spoof")), "spoofing guest captured guest A's frame")

	// Neither may a guest on a port that still learns, like a TAP created
	// before hardening.
	sendFrame(t, recvControl, control, net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, []byte("spoof"), macA)
	sendFrame(t, send, br, macA, []byte("after-learning-spoof"))
	assert.True(t, receivedFrame(recvA, []byte("after-learning-spoof")), "guest A lost delivery after spoof from a learning port")
	assert.False(t, receivedFrame(recvControl, []byte("after-learning-spoof")), "learning port captured guest A's frame")
}

// addBridgePort creates a veth pair, enslaves one end to br as an isolated port
// and returns the other end, which stands in for the guest NIC.
func addBridgePort(t *testing.T, br netlink.Link, name string, defaults bool) netlink.Link {
	t.Helper()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "port-" + name}, PeerName: "guest-" + name}
	require.NoError(t, netlink.LinkAdd(veth))
	port, err := netlink.LinkByName("port-" + name)
	require.NoError(t, err)
	guest, err := netlink.LinkByName("guest-" + name)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetMaster(port, br))
	require.NoError(t, netlink.LinkSetIsolated(port, true))
	if !defaults {
		require.NoError(t, hardenIsolatedPort(port, guest.Attrs().HardwareAddr))

		neighs, err := netlink.NeighList(port.Attrs().Index, unix.AF_BRIDGE)
		require.NoError(t, err)
		var pinned *netlink.Neigh
		for i := range neighs {
			if bytes.Equal(neighs[i].HardwareAddr, guest.Attrs().HardwareAddr) {
				pinned = &neighs[i]
			}
		}
		require.NotNil(t, pinned, "guest MAC not in bridge FDB")
		assert.Equal(t, netlink.NUD_NOARP, pinned.State, "FDB entry should be static, not local")
		assert.NotZero(t, pinned.Flags&netlink.NTF_STICKY, "FDB entry should be sticky")
	}
	require.NoError(t, netlink.LinkSetUp(port))
	require.NoError(t, netlink.LinkSetUp(guest))
	return guest
}

// waitUp waits for the named links to come up. Bridge ports with STP off start
// forwarding once they have carrier. It polls on the calling goroutine because
// the links only exist in this thread's network namespace.
func waitUp(t *testing.T, names ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, name := range names {
		for {
			link, err := netlink.LinkByName(name)
			require.NoError(t, err)
			if link.Attrs().OperState == netlink.OperUp {
				break
			}
			require.True(t, time.Now().Before(deadline), "%s did not come up", name)
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func openPacketSocket(t *testing.T, link netlink.Link) int {
	t.Helper()
	return openPacketSocketFor(t, link, testEtherType)
}

func openPacketSocketFor(t *testing.T, link netlink.Link, etherType uint16) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(etherType)))
	require.NoError(t, err)
	t.Cleanup(func() { unix.Close(fd) })
	require.NoError(t, unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(etherType), Ifindex: link.Attrs().Index}))
	tv := unix.NsecToTimeval((200 * time.Millisecond).Nanoseconds())
	require.NoError(t, unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv))
	return fd
}

func sendFrame(t *testing.T, fd int, link netlink.Link, dst net.HardwareAddr, payload []byte, src ...net.HardwareAddr) {
	t.Helper()
	from := link.Attrs().HardwareAddr
	if len(src) > 0 {
		from = src[0]
	}
	frame := append(append(append([]byte{}, dst...), from...), byte(testEtherType>>8), byte(testEtherType&0xff))
	frame = append(frame, payload...)
	for len(frame) < 60 {
		frame = append(frame, 0)
	}
	require.NoError(t, unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Protocol: htons(testEtherType), Ifindex: link.Attrs().Index, Halen: 6}))
}

// receivedFrame reports whether a frame carrying payload arrives before the
// socket's read timeout.
func receivedFrame(fd int, payload []byte) bool {
	buf := make([]byte, 2048)
	for {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		if n > 14 && bytes.HasPrefix(buf[14:n], payload) {
			return true
		}
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

var (
	bridgeMAC = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	bridgeIP  = net.IPv4(10, 0, 0, 1).To4()
	guestAIP  = net.IPv4(10, 0, 0, 2).To4()
	guestBIP  = net.IPv4(10, 0, 0, 3).To4()
)

// TestPinnedNeighborIgnoresForgedARP checks that a guest behind a port without
// source filtering, like a TAP created before it existed, cannot take over
// another guest's address in the host's neighbor table.
func TestPinnedNeighborIgnoresForgedARP(t *testing.T) {
	br := enterGuestNetns(t)
	// Let an unsolicited reply override an entry at once, so only the pin
	// keeps it.
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv4/neigh/br0/locktime", []byte("0"), 0o644))

	guestA := addBridgePort(t, br, "a", false)
	guestC := addBridgePort(t, br, "c", true)
	macA := guestA.Attrs().HardwareAddr

	// The host already resolved guest A before the forged ARP arrives.
	require.NoError(t, netlink.NeighSet(&netlink.Neigh{
		LinkIndex:    br.Attrs().Index,
		Family:       netlink.FAMILY_V4,
		State:        netlink.NUD_STALE,
		IP:           guestAIP,
		HardwareAddr: macA,
	}))
	portA, err := netlink.LinkByName("port-a")
	require.NoError(t, err)
	require.NoError(t, filterGuestSource(portA, macA, guestAIP))
	require.NoError(t, (&manager{}).pinNeighbor(br, guestAIP, macA))

	waitUp(t, "br0", "port-a", "port-c", "guest-a", "guest-c")
	recvA := openPacketSocketFor(t, guestA, unix.ETH_P_IP)
	recvC := openPacketSocketFor(t, guestC, unix.ETH_P_IP)
	sendC := openPacketSocketFor(t, guestC, unix.ETH_P_ARP)

	macC := guestC.Attrs().HardwareAddr
	broadcast := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	sendRawFrame(t, sendC, guestC, arpFrame(broadcast, macC, 1, macC, guestAIP, make(net.HardwareAddr, 6), guestAIP, nil))
	sendRawFrame(t, sendC, guestC, arpFrame(bridgeMAC, macC, 2, macC, guestAIP, bridgeMAC, bridgeIP, nil))
	time.Sleep(100 * time.Millisecond)

	entry := neighborFor(t, br, guestAIP)
	require.NotNil(t, entry, "guest A's neighbor entry is gone")
	assert.Equal(t, netlink.NUD_PERMANENT, entry.State)
	assert.Equal(t, macA, entry.HardwareAddr, "forged ARP moved guest A's address to another MAC")

	sendUDP(t, guestAIP, []byte("host-to-a"))
	assert.True(t, receivedMarker(recvA, []byte("host-to-a")), "guest A did not receive host traffic")
	assert.False(t, receivedMarker(recvC, []byte("host-to-a")), "host traffic for guest A reached another guest")
}

// TestGuestSourceFilterDropsSpoofedFrames sends frames from a filtered guest
// port and checks which of them reach the host.
func TestGuestSourceFilterDropsSpoofedFrames(t *testing.T) {
	br := enterGuestNetns(t)
	guest := addBridgePort(t, br, "b", false)
	port, err := netlink.LinkByName("port-b")
	require.NoError(t, err)
	mac := guest.Attrs().HardwareAddr
	require.NoError(t, filterGuestSource(port, mac, guestBIP))

	waitUp(t, "br0", "port-b", "guest-b")
	host := openPacketSocketFor(t, br, unix.ETH_P_ALL)
	send := openPacketSocketFor(t, guest, unix.ETH_P_ALL)

	other := net.HardwareAddr{0x02, 0x00, 0x00, 0xde, 0xad, 0x02}
	broadcast := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	noMAC := make(net.HardwareAddr, 6)
	cases := []struct {
		name  string
		frame func(marker []byte) []byte
		pass  bool
	}{
		{"arp", func(m []byte) []byte { return arpFrame(broadcast, mac, 1, mac, guestBIP, noMAC, bridgeIP, m) }, true},
		{"ipv4", func(m []byte) []byte { return ipv4Frame(bridgeMAC, mac, guestBIP, m) }, true},
		{"ipv4 spoofed source MAC", func(m []byte) []byte { return ipv4Frame(bridgeMAC, other, guestBIP, m) }, false},
		{"ipv4 spoofed source IP", func(m []byte) []byte { return ipv4Frame(bridgeMAC, mac, guestAIP, m) }, false},
		{"arp spoofed source MAC", func(m []byte) []byte { return arpFrame(broadcast, other, 1, mac, guestBIP, noMAC, bridgeIP, m) }, false},
		{"arp forged sender MAC", func(m []byte) []byte { return arpFrame(broadcast, mac, 1, other, guestBIP, noMAC, bridgeIP, m) }, false},
		{"arp forged sender IP", func(m []byte) []byte { return arpFrame(broadcast, mac, 1, mac, guestAIP, noMAC, guestAIP, m) }, false},
		{"ipv6", func(m []byte) []byte { return ipv6Frame(bridgeMAC, mac, m) }, false},
		{"other ethertype", func(m []byte) []byte {
			return append(append(append([]byte{}, bridgeMAC...), mac...), append([]byte{testEtherType >> 8, testEtherType & 0xff}, m...)...)
		}, false},
	}
	for _, c := range cases {
		marker := []byte("marker:" + c.name)
		sendRawFrame(t, send, guest, c.frame(marker))
		assert.Equal(t, c.pass, receivedMarker(host, marker), "%s: passed=%v", c.name, !c.pass)
	}

	// Frames too short to hold the fields the filters read, sent unpadded
	// and marked by their spoofed source MAC.
	for payload := 0; payload < 4; payload++ {
		runtMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0xde, 0xad, byte(0x10 + payload)}
		frame := append(append(append([]byte{}, bridgeMAC...), runtMAC...), 0x08, 0x00)
		frame = append(frame, make([]byte, payload)...)
		require.NoError(t, unix.Sendto(send, frame, 0, &unix.SockaddrLinklayer{Ifindex: guest.Attrs().Index, Halen: 6}))
		assert.False(t, receivedMarker(host, runtMAC), "%d-byte frame passed", len(frame))
	}
}

// TestReleasedAddressKeepsNoPinnedNeighbor drives TAP setup and every release
// path through the manager and checks the host's neighbor entries.
func TestReleasedAddressKeepsNoPinnedNeighbor(t *testing.T) {
	br := enterGuestNetns(t)
	m := &manager{
		paths:              paths.New(t.TempDir()),
		config:             &config.Config{Network: config.NetworkConfig{BridgeName: "br0"}},
		pendingAllocations: make(map[string]pendingAllocation),
	}
	m.setDefaultNetwork(newDefaultNetwork("br0", "10.0.0.0/24", bridgeIP.String()))
	ctx := context.Background()

	const idA, idB = "pinaaaaa", "pinbbbbb"
	macA := writeGuestMetadata(t, m, idA, guestAIP, "02:00:00:00:0a:0a")
	macB := writeGuestMetadata(t, m, idB, guestAIP, "02:00:00:00:0b:0b")

	require.NoError(t, m.RecreateAllocation(ctx, idA, 0, 0))
	tapA, err := netlink.LinkByName(GenerateTAPName(idA))
	require.NoError(t, err)
	filters, err := netlink.FilterList(tapA, netlink.HANDLE_MIN_INGRESS)
	require.NoError(t, err)
	assert.Len(t, filters, 3, "guest source filters on the TAP")
	assertPinned(t, br, guestAIP, macA)

	allocA, err := m.GetAllocation(ctx, idA)
	require.NoError(t, err)
	require.NoError(t, m.ReleaseAllocation(ctx, allocA))
	assert.Nil(t, neighborFor(t, br, guestAIP), "released address kept its neighbor entry")
	qdiscs, err := netlink.QdiscList(nil)
	require.NoError(t, err)
	for _, q := range qdiscs {
		assert.NotEqual(t, tapA.Attrs().Index, q.Attrs().LinkIndex, "qdisc left on deleted TAP")
	}

	// Guest B is handed the address while guest A's TAP is still around, as
	// when an orphaned TAP outlives its instance.
	require.NoError(t, m.RecreateAllocation(ctx, idA, 0, 0))
	require.NoError(t, m.RecreateAllocation(ctx, idB, 0, 0))
	assertPinned(t, br, guestAIP, macB)

	assert.Equal(t, 1, m.CleanupOrphanedTAPs(ctx, []string{idB}, 0))
	assertPinned(t, br, guestAIP, macB)

	require.NoError(t, m.ReleaseByInstanceID(ctx, idB))
	assert.Nil(t, neighborFor(t, br, guestAIP), "released address kept its neighbor entry")
}

// enterGuestNetns moves the test into a throwaway network namespace with a
// bridge that has a fixed MAC and the gateway address. The kernel flushes a
// bridge's neighbor entries when its MAC changes, which hosts avoid by fixing
// the bridge MAC.
func enterGuestNetns(t *testing.T) netlink.Link {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("Skipping test that requires root")
	}
	runtime.LockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	ns, err := netns.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, netns.Set(orig))
		ns.Close()
		orig.Close()
		runtime.UnlockOSThread()
	})

	require.NoError(t, netlink.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}))
	br, err := netlink.LinkByName("br0")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetHardwareAddr(br, bridgeMAC))
	require.NoError(t, netlink.AddrAdd(br, &netlink.Addr{IPNet: &net.IPNet{IP: bridgeIP, Mask: net.CIDRMask(24, 32)}}))
	require.NoError(t, netlink.LinkSetUp(br))
	return br
}

func writeGuestMetadata(t *testing.T, m *manager, id string, ip net.IP, mac string) net.HardwareAddr {
	t.Helper()
	data, err := json.Marshal(instanceMetadata{Name: id, NetworkEnabled: true, IP: ip.String(), MAC: mac})
	require.NoError(t, err)
	path := m.paths.InstanceMetadata(id)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o644))
	hw, err := net.ParseMAC(mac)
	require.NoError(t, err)
	return hw
}

func neighborFor(t *testing.T, br netlink.Link, ip net.IP) *netlink.Neigh {
	t.Helper()
	neighbors, err := netlink.NeighList(br.Attrs().Index, netlink.FAMILY_V4)
	require.NoError(t, err)
	for i := range neighbors {
		if neighbors[i].IP.Equal(ip) {
			return &neighbors[i]
		}
	}
	return nil
}

func assertPinned(t *testing.T, br netlink.Link, ip net.IP, mac net.HardwareAddr) {
	t.Helper()
	entry := neighborFor(t, br, ip)
	if assert.NotNil(t, entry, "no neighbor entry for %s", ip) {
		assert.Equal(t, netlink.NUD_PERMANENT, entry.State, "neighbor entry for %s", ip)
		assert.Equal(t, mac, entry.HardwareAddr, "neighbor entry for %s", ip)
	}
}

func arpFrame(dst, src net.HardwareAddr, op uint16, sha net.HardwareAddr, spa net.IP, tha net.HardwareAddr, tpa net.IP, marker []byte) []byte {
	frame := append(append(append([]byte{}, dst...), src...), 0x08, 0x06)
	frame = append(frame, 0, 1, 0x08, 0x00, 6, 4, byte(op>>8), byte(op))
	frame = append(append(append(append(append(frame, sha...), spa...), tha...), tpa...), marker...)
	return frame
}

func ipv4Frame(dst, src net.HardwareAddr, srcIP net.IP, marker []byte) []byte {
	frame := append(append(append([]byte{}, dst...), src...), 0x08, 0x00)
	length := 20 + len(marker)
	frame = append(frame, 0x45, 0, byte(length>>8), byte(length), 0, 0, 0, 0, 64, unix.IPPROTO_UDP, 0, 0)
	return append(append(append(frame, srcIP...), bridgeIP...), marker...)
}

func ipv6Frame(dst, src net.HardwareAddr, marker []byte) []byte {
	frame := append(append(append([]byte{}, dst...), src...), 0x86, 0xdd)
	frame = append(frame, 0x60, 0, 0, 0, byte(len(marker)>>8), byte(len(marker)), unix.IPPROTO_UDP, 64)
	frame = append(frame, net.ParseIP("fe80::2")...)
	frame = append(frame, net.ParseIP("fe80::1")...)
	return append(frame, marker...)
}

func sendRawFrame(t *testing.T, fd int, link netlink.Link, frame []byte) {
	t.Helper()
	for len(frame) < 60 {
		frame = append(frame, 0)
	}
	require.NoError(t, unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: link.Attrs().Index, Halen: 6}))
}

func sendUDP(t *testing.T, dst net.IP, payload []byte) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	require.NoError(t, err)
	defer unix.Close(fd)
	to := &unix.SockaddrInet4{Port: 9}
	copy(to.Addr[:], dst.To4())
	require.NoError(t, unix.Sendto(fd, payload, 0, to))
}

// receivedMarker reports whether an incoming frame containing marker arrives
// before the socket's read timeout.
func receivedMarker(fd int, marker []byte) bool {
	buf := make([]byte, 2048)
	for {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		if bytes.Contains(buf[:n], marker) {
			return true
		}
	}
}
