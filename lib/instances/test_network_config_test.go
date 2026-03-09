package instances

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
)

var testNetworkSeq atomic.Uint32
var testNetworkByName sync.Map
var testNetworkRunSeed = uint32(time.Now().UnixNano()) ^ uint32(os.Getpid()<<8)
var testNetworkGuardCleanupOnce sync.Once

const (
	testSubnetSecondOctetMin = 200
	testSubnetSecondOctetMax = 249
	testSubnetThirdOctetMin  = 1
	testSubnetThirdOctetMax  = 250
)

type testNetworkLease struct {
	cfg     config.NetworkConfig
	release func()
}

type subnetLeaseFile struct {
	Leases map[string]subnetLease `json:"leases"`
}

type subnetLease struct {
	TestName   string `json:"test_name"`
	BridgeName string `json:"bridge_name"`
	SubnetCIDR string `json:"subnet_cidr"`
	PID        int    `json:"pid"`
	CreatedAt  int64  `json:"created_at_unix"`
}

type hostRoute struct {
	cidr     string
	network  *net.IPNet
	device   string
	linkDown bool
}

var errRouteCommandUnavailable = errors.New("ip route command unavailable")

func newParallelTestNetworkConfig(t *testing.T) config.NetworkConfig {
	t.Helper()

	if existing, ok := testNetworkByName.Load(t.Name()); ok {
		return existing.(*testNetworkLease).cfg
	}

	seq := testNetworkSeq.Add(1)
	lease, err := allocateTestNetworkLease(t.Name(), seq)
	if err != nil {
		t.Fatalf("allocate test network config: %v", err)
	}

	actual, loaded := testNetworkByName.LoadOrStore(t.Name(), lease)
	if loaded {
		lease.release()
		return actual.(*testNetworkLease).cfg
	}

	t.Cleanup(lease.release)
	return lease.cfg
}

func allocateTestNetworkLease(testName string, seq uint32) (*testNetworkLease, error) {
	if runtime.GOOS != "linux" {
		return &testNetworkLease{
			cfg:     legacyParallelTestNetworkConfig(seq),
			release: func() {},
		}, nil
	}

	var allocatedSubnet string
	var bridgeName string
	var cfg config.NetworkConfig

	err := withTestSubnetLock(func() error {
		routes, err := listHostRoutes()
		if err != nil {
			return err
		}

		testNetworkGuardCleanupOnce.Do(func() {
			cleanupStaleLinkDownRoutes(routes)
			// Refresh route snapshot after cleanup so subnet selection sees current state.
			refreshed, refreshErr := listHostRoutes()
			if refreshErr == nil {
				routes = refreshed
			}
		})

		leases, err := loadSubnetLeases()
		if err != nil {
			return err
		}

		pruneStaleLeases(leases, routes)
		if err := saveSubnetLeases(leases); err != nil {
			return err
		}

		startIdx := int((testNetworkRunSeed + seq - 1) % uint32(testSubnetSpaceSize()))
		subnet, err := findFreeTestSubnet(startIdx, routes, leases)
		if err != nil {
			return err
		}

		bridgeName = fmt.Sprintf("hm%04x%03x", testNetworkRunSeed&0xffff, seq%0xfff)
		allocatedSubnet = subnet
		leases[subnet] = subnetLease{
			TestName:   testName,
			BridgeName: bridgeName,
			SubnetCIDR: subnet,
			PID:        os.Getpid(),
			CreatedAt:  time.Now().Unix(),
		}

		if err := saveSubnetLeases(leases); err != nil {
			return err
		}

		cfg = config.NetworkConfig{
			BridgeName: bridgeName,
			SubnetCIDR: subnet,
			DNSServer:  "1.1.1.1",
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errRouteCommandUnavailable) {
			return &testNetworkLease{
				cfg:     legacyParallelTestNetworkConfig(seq),
				release: func() {},
			}, nil
		}
		return nil, err
	}

	var releaseOnce sync.Once
	return &testNetworkLease{
		cfg: cfg,
		release: func() {
			releaseOnce.Do(func() {
				_ = withTestSubnetLock(func() error {
					cleanupTestNetworkArtifacts(bridgeName, allocatedSubnet)

					leases, err := loadSubnetLeases()
					if err != nil {
						return nil
					}
					delete(leases, allocatedSubnet)
					if err := saveSubnetLeases(leases); err != nil {
						return nil
					}
					return nil
				})
			})
		},
	}, nil
}

func withTestSubnetLock(fn func() error) error {
	lockPath := filepath.Join(os.TempDir(), "hypeman-test-network.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open subnet lock file: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire subnet lock: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	return fn()
}

func testSubnetLeaseFilePath() string {
	return filepath.Join(os.TempDir(), "hypeman-test-network-leases.json")
}

func loadSubnetLeases() (map[string]subnetLease, error) {
	path := testSubnetLeaseFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]subnetLease), nil
		}
		return nil, fmt.Errorf("read subnet lease file: %w", err)
	}

	var leases subnetLeaseFile
	if len(data) > 0 {
		if err := json.Unmarshal(data, &leases); err != nil {
			return nil, fmt.Errorf("unmarshal subnet leases: %w", err)
		}
	}
	if leases.Leases == nil {
		leases.Leases = make(map[string]subnetLease)
	}
	return leases.Leases, nil
}

func saveSubnetLeases(leases map[string]subnetLease) error {
	leaseState := subnetLeaseFile{Leases: leases}
	data, err := json.Marshal(leaseState)
	if err != nil {
		return fmt.Errorf("marshal subnet leases: %w", err)
	}

	path := testSubnetLeaseFilePath()
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write subnet lease temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename subnet lease file: %w", err)
	}
	return nil
}

func listHostRoutes() ([]hostRoute, error) {
	cmd := exec.Command("ip", "-4", "route", "show")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errRouteCommandUnavailable
		}
		return nil, fmt.Errorf("list host routes: %w", err)
	}

	lines := strings.Split(string(out), "\n")
	routes := make([]hostRoute, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "default ") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		_, network, err := net.ParseCIDR(fields[0])
		if err != nil {
			continue
		}

		route := hostRoute{
			cidr:     network.String(),
			network:  network,
			linkDown: strings.Contains(line, " linkdown"),
		}
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "dev" {
				route.device = fields[i+1]
				break
			}
		}
		routes = append(routes, route)
	}

	return routes, nil
}

func cleanupStaleLinkDownRoutes(routes []hostRoute) {
	for _, route := range routes {
		if !route.linkDown {
			continue
		}
		if !isTestCIDR(route.cidr) {
			continue
		}
		if !strings.HasPrefix(route.device, "hm") && !strings.HasPrefix(route.device, "ha") {
			continue
		}

		cleanupTestNetworkArtifacts(route.device, route.cidr)
	}
}

func pruneStaleLeases(leases map[string]subnetLease, routes []hostRoute) {
	liveRoutes := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		liveRoutes[route.cidr] = struct{}{}
	}

	for subnet, lease := range leases {
		_, hasRoute := liveRoutes[subnet]
		if hasRoute {
			continue
		}
		if bridgeExists(lease.BridgeName) {
			continue
		}
		delete(leases, subnet)
	}
}

func bridgeExists(name string) bool {
	if name == "" {
		return false
	}
	cmd := exec.Command("ip", "link", "show", "dev", name)
	return cmd.Run() == nil
}

func findFreeTestSubnet(startIdx int, routes []hostRoute, leases map[string]subnetLease) (string, error) {
	testRoutes := make([]*net.IPNet, 0, len(routes))
	for _, route := range routes {
		testRoutes = append(testRoutes, route.network)
	}

	subnetSpace := testSubnetSpaceSize()
	for offset := 0; offset < subnetSpace; offset++ {
		idx := (startIdx + offset) % subnetSpace
		subnet := testSubnetAt(idx)
		if _, exists := leases[subnet]; exists {
			continue
		}

		_, candidateNet, err := net.ParseCIDR(subnet)
		if err != nil {
			continue
		}

		conflicts := false
		for _, route := range testRoutes {
			if route == nil {
				continue
			}
			if cidrOverlaps(candidateNet, route) {
				conflicts = true
				break
			}
		}
		if conflicts {
			continue
		}

		return subnet, nil
	}

	return "", fmt.Errorf("no free subnet available in test range 10.%d-%d.%d-%d.0/24",
		testSubnetSecondOctetMin, testSubnetSecondOctetMax, testSubnetThirdOctetMin, testSubnetThirdOctetMax)
}

func testSubnetSpaceSize() int {
	return (testSubnetSecondOctetMax - testSubnetSecondOctetMin + 1) * (testSubnetThirdOctetMax - testSubnetThirdOctetMin + 1)
}

func testSubnetAt(idx int) string {
	thirdRangeSize := testSubnetThirdOctetMax - testSubnetThirdOctetMin + 1
	secondOctet := testSubnetSecondOctetMin + (idx / thirdRangeSize)
	thirdOctet := testSubnetThirdOctetMin + (idx % thirdRangeSize)
	return fmt.Sprintf("10.%d.%d.0/24", secondOctet, thirdOctet)
}

func cidrOverlaps(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func isTestCIDR(cidr string) bool {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil || ip == nil || network == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4[0] != 10 {
		return false
	}
	return int(ip4[1]) >= testSubnetSecondOctetMin && int(ip4[1]) <= testSubnetSecondOctetMax
}

func cleanupTestNetworkArtifacts(bridgeName, subnetCIDR string) {
	if subnetCIDR != "" && bridgeName != "" {
		_ = exec.Command("ip", "-4", "route", "del", subnetCIDR, "dev", bridgeName).Run()
	}
	if bridgeName != "" {
		_ = exec.Command("ip", "link", "delete", bridgeName, "type", "bridge").Run()
	}

	bridgeSuffix := strings.ToLower(bridgeName)
	deleteIPTablesRulesByComment("nat", "POSTROUTING", "hypeman-nat-"+bridgeSuffix)
	deleteIPTablesRulesByComment("", "FORWARD", "hypeman-fwd-out-"+bridgeSuffix)
	deleteIPTablesRulesByComment("", "FORWARD", "hypeman-fwd-in-"+bridgeSuffix)
}

func deleteIPTablesRulesByComment(table, chain, comment string) {
	if comment == "" {
		return
	}

	args := []string{}
	if table != "" {
		args = append(args, "-t", table)
	}
	args = append(args, "-L", chain, "--line-numbers", "-n")
	listCmd := exec.Command("iptables", args...)
	output, err := listCmd.Output()
	if err != nil {
		return
	}

	var ruleNums []int
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, comment) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ruleNum, convErr := strconv.Atoi(fields[0])
		if convErr != nil {
			continue
		}
		ruleNums = append(ruleNums, ruleNum)
	}

	for i := len(ruleNums) - 1; i >= 0; i-- {
		delArgs := []string{}
		if table != "" {
			delArgs = append(delArgs, "-t", table)
		}
		delArgs = append(delArgs, "-D", chain, strconv.Itoa(ruleNums[i]))
		_ = exec.Command("iptables", delArgs...).Run()
	}
}

func legacyParallelTestNetworkConfig(seq uint32) config.NetworkConfig {
	const subnetSpace = 50 * 250 // second octet 200-249, third octet 1-250
	subnetIdx := (testNetworkRunSeed + seq - 1) % subnetSpace
	bridge := fmt.Sprintf("hm%04x%03x", testNetworkRunSeed&0xffff, seq%0xfff)
	secondOctet := 200 + int(subnetIdx/250)
	thirdOctet := int((subnetIdx % 250) + 1)
	return config.NetworkConfig{
		BridgeName: bridge,
		SubnetCIDR: fmt.Sprintf("10.%d.%d.0/24", secondOctet, thirdOctet),
		DNSServer:  "1.1.1.1",
	}
}
