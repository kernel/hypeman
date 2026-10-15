package integration

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/network/testsupport"
)

var testNetworkSeq atomic.Uint32
var testNetworkByName sync.Map
var testNetworkRunSeed = uint32(time.Now().UnixNano()) ^ uint32(os.Getpid()<<8)

func newParallelTestNetworkConfig(t *testing.T) config.NetworkConfig {
	t.Helper()

	testsupport.SweepOrphanedRulesOnce()

	if cfg, ok := testNetworkByName.Load(t.Name()); ok {
		return cfg.(config.NetworkConfig)
	}

	seq := testNetworkSeq.Add(1)
	const subnetSpace = 50 * 250 // second octet 200-249, third octet 1-250
	subnetIdx := (testNetworkRunSeed + seq - 1) % subnetSpace

	bridge := fmt.Sprintf("hi%04x%03x", testNetworkRunSeed&0xffff, seq%0xfff)
	secondOctet := 200 + int((subnetIdx / 250))
	thirdOctet := int((subnetIdx % 250) + 1)

	cfg := config.NetworkConfig{
		BridgeName: bridge,
		SubnetCIDR: fmt.Sprintf("10.%d.%d.0/24", secondOctet, thirdOctet),
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
