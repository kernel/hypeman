package integration

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/network/testsupport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNetworkSeq atomic.Uint32
var testNetworkByName sync.Map
var testNetworkRunSeed = uint32(time.Now().UnixNano()) ^ uint32(os.Getpid()<<8)

const testSubnetSpaceSize = 50 * 250 // second octet 200-249, third octet 1-250

// testSubnetMu guards testSubnetsInUse. It only orders allocations within this
// test process; it is not a host-wide lock.
var testSubnetMu sync.Mutex
var testSubnetsInUse = map[string]struct{}{}

func newParallelTestNetworkConfig(t *testing.T) config.NetworkConfig {
	t.Helper()

	testsupport.SweepOrphanedRulesOnce()

	if cfg, ok := testNetworkByName.Load(t.Name()); ok {
		return cfg.(config.NetworkConfig)
	}

	seq := testNetworkSeq.Add(1)
	bridge := fmt.Sprintf("hi%04x%03x", testNetworkRunSeed&0xffff, seq%0xfff)
	cfg := config.NetworkConfig{
		BridgeName: bridge,
		SubnetCIDR: allocateTestSubnet(t, testNetworkRunSeed+seq-1),
		DNSServer:  "1.1.1.1",
	}

	actual, loaded := testNetworkByName.LoadOrStore(t.Name(), cfg)
	if !loaded {
		// Nothing else removes the bridge the network manager creates for this
		// config, so delete it with the test.
		t.Cleanup(func() {
			testsupport.DeleteBridge(bridge)
			testNetworkByName.Delete(t.Name())
		})
	}
	return actual.(config.NetworkConfig)
}

// allocateTestSubnet returns a /24 that overlaps no existing host route and no
// subnet already handed out by this process. Routes left behind by killed runs
// are skipped, which is what made the fixed-formula allocator collide.
func allocateTestSubnet(t *testing.T, start uint32) string {
	t.Helper()

	routes := hostRouteNetworks(t)

	testSubnetMu.Lock()
	defer testSubnetMu.Unlock()

	cidr, ok := pickTestSubnet(start, routes, testSubnetsInUse)
	if !ok {
		t.Fatalf("no free test subnet in 10.200-249.1-250.0/24")
	}
	testSubnetsInUse[cidr] = struct{}{}
	return cidr
}

// pickTestSubnet scans the test range from start and returns the first /24
// that is not in inUse and does not overlap any route.
func pickTestSubnet(start uint32, routes []*net.IPNet, inUse map[string]struct{}) (string, bool) {
	for offset := 0; offset < testSubnetSpaceSize; offset++ {
		idx := (int(start) + offset) % testSubnetSpaceSize
		secondOctet := 200 + idx/250
		thirdOctet := idx%250 + 1
		cidr := fmt.Sprintf("10.%d.%d.0/24", secondOctet, thirdOctet)
		if _, taken := inUse[cidr]; taken {
			continue
		}
		_, candidate, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if overlapsAny(candidate, routes) {
			continue
		}
		return cidr, true
	}
	return "", false
}

func overlapsAny(candidate *net.IPNet, routes []*net.IPNet) bool {
	for _, route := range routes {
		if candidate.Contains(route.IP) || route.Contains(candidate.IP) {
			return true
		}
	}
	return false
}

// hostRouteNetworks lists IPv4 route destinations. If ip is unavailable it
// returns nothing, which matches the allocator's behavior before route checks.
func hostRouteNetworks(t *testing.T) []*net.IPNet {
	t.Helper()

	out, err := exec.Command("ip", "-4", "route", "show").Output()
	if err != nil {
		t.Logf("list host routes for subnet allocation: %v", err)
		return nil
	}

	var routes []*net.IPNet
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] == "default" {
			continue
		}
		if _, network, err := net.ParseCIDR(fields[0]); err == nil {
			routes = append(routes, network)
		} else if ip := net.ParseIP(fields[0]); ip != nil {
			// Single-host routes print without a prefix length.
			routes = append(routes, &net.IPNet{IP: ip, Mask: net.CIDRMask(len(ip)*8, len(ip)*8)})
		}
	}
	return routes
}

func TestPickTestSubnetSkipsRoutesAndInUse(t *testing.T) {
	_, leaked, err := net.ParseCIDR("10.200.1.0/24")
	require.NoError(t, err)
	_, hostRoute, err := net.ParseCIDR("10.200.2.7/32")
	require.NoError(t, err)
	inUse := map[string]struct{}{"10.200.3.0/24": {}}

	cidr, ok := pickTestSubnet(0, []*net.IPNet{leaked, hostRoute}, inUse)

	require.True(t, ok)
	assert.Equal(t, "10.200.4.0/24", cidr)
}

func TestPickTestSubnetWrapsAroundSpace(t *testing.T) {
	last := uint32(testSubnetSpaceSize - 1)

	inUse := map[string]struct{}{"10.249.250.0/24": {}}

	cidr, ok := pickTestSubnet(last, nil, inUse)

	require.True(t, ok)
	assert.Equal(t, "10.200.1.0/24", cidr)
}
