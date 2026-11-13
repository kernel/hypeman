//go:build linux

package network

import (
	"bytes"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

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
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(testEtherType)))
	require.NoError(t, err)
	t.Cleanup(func() { unix.Close(fd) })
	require.NoError(t, unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(testEtherType), Ifindex: link.Attrs().Index}))
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
