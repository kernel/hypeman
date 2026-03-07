package api

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
)

var testNetworkSeq atomic.Uint32
var testNetworkByName sync.Map

func newParallelTestNetworkConfig(t *testing.T) config.NetworkConfig {
	t.Helper()

	if cfg, ok := testNetworkByName.Load(t.Name()); ok {
		return cfg.(config.NetworkConfig)
	}

	seq := testNetworkSeq.Add(1)
	pid := uint32(os.Getpid())

	bridge := fmt.Sprintf("ha%04x%03x", pid&0xffff, seq%0xfff)
	secondOctet := int(((pid >> 4) % 100) + 100) // 100-199
	thirdOctet := int((seq % 250) + 1)           // 1-250

	cfg := config.NetworkConfig{
		BridgeName: bridge,
		SubnetCIDR: fmt.Sprintf("10.%d.%d.0/24", secondOctet, thirdOctet),
		DNSServer:  "1.1.1.1",
	}

	actual, _ := testNetworkByName.LoadOrStore(t.Name(), cfg)
	return actual.(config.NetworkConfig)
}
