package instances

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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

// iptablesWaitSeconds matches the production convention in
// lib/network/bridge_linux.go so the test harness cooperates with the
// system-wide xtables lock instead of failing immediately under contention.
const testIPTablesWaitSeconds = "5"

// iptablesDeleteAttempts is the number of times we retry an iptables delete on
// transient lock contention before giving up.
const iptablesDeleteAttempts = 3

// newTestIPTablesCommand builds an iptables command with the -w wait flag so it
// waits for the xtables lock rather than failing immediately.
func newTestIPTablesCommand(args ...string) *exec.Cmd {
	fullArgs := make([]string, 0, len(args)+2)
	fullArgs = append(fullArgs, "-w", testIPTablesWaitSeconds)
	fullArgs = append(fullArgs, args...)
	return exec.Command("iptables", fullArgs...)
}

// logTestNetworkErr surfaces a real cleanup failure to stderr. These helpers run
// both with and without a *testing.T (e.g. under testNetworkGuardCleanupOnce and
// in t.Cleanup), so we log instead of swallowing the error silently.
func logTestNetworkErr(op string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "hypeman-test-network: %s: %v\n", op, err)
}

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

	testName := t.Name()
	if existing, ok := testNetworkByName.Load(testName); ok {
		return existing.(*testNetworkLease).cfg
	}

	seq := testNetworkSeq.Add(1)
	lease, err := allocateTestNetworkLease(testName, seq)
	if err != nil {
		t.Fatalf("allocate test network config: %v", err)
	}

	actual, loaded := testNetworkByName.LoadOrStore(testName, lease)
	if loaded {
		lease.release()
		return actual.(*testNetworkLease).cfg
	}

	t.Cleanup(func() {
		lease.release()
		testNetworkByName.Delete(testName)
	})
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

	if routes, err := listHostRoutes(); err == nil {
		cleanupStaleTestNetworks(routes)
	}

	err := withTestSubnetLock(func() error {
		routes, err := listHostRoutes()
		if err != nil {
			return err
		}

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
				cleanupTestNetworkArtifacts(bridgeName, allocatedSubnet)

				_ = withTestSubnetLock(func() error {
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

func cleanupStaleTestNetworks(routes []hostRoute) {
	testNetworkGuardCleanupOnce.Do(func() {
		lockPath := filepath.Join(os.TempDir(), "hypeman-test-network-cleanup.lock")
		_, err := tryWithTestFileLock(lockPath, func() error {
			cleanupStaleLinkDownRoutes(routes)
			// Sweep iptables rules for test bridges that no longer exist. Once a
			// bridge is fully deleted its route is gone too, so linkdown cleanup
			// above can't catch these — they would otherwise leak forever.
			sweepOrphanedTestIPTablesRules()
			return nil
		})
		logTestNetworkErr("stale network cleanup", err)
	})
}

func withTestSubnetLock(fn func() error) error {
	lockPath := filepath.Join(os.TempDir(), "hypeman-test-network.lock")
	lockFile, err := openTestLockFile(lockPath)
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

func tryWithTestFileLock(lockPath string, fn func() error) (bool, error) {
	lockFile, err := openTestLockFile(lockPath)
	if err != nil {
		return false, fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("acquire lock: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	return true, fn()
}

func openTestLockFile(lockPath string) (*os.File, error) {
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err == nil {
		_ = lockFile.Chmod(0o666)
		return lockFile, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	lockFile, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o666)
	if errors.Is(err, os.ErrExist) {
		return os.OpenFile(lockPath, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, err
	}
	_ = lockFile.Chmod(0o666)
	return lockFile, nil
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

// testRuleCommentPattern matches hypeman test-harness iptables rule comments and
// captures both the full comment and the referenced bridge name. We deliberately
// anchor on the "hm" test prefix so we never touch "ha"-prefixed rules from a
// real (non-test) hypeman process running on the same host.
var testRuleCommentPattern = regexp.MustCompile(`^hypeman-(?:fwd-out|fwd-in|nat)-(hm[0-9a-f]+)$`)

// sweepOrphanedTestIPTablesRules removes hypeman test iptables rules whose
// referenced bridge interface no longer exists. Once a bridge is fully deleted,
// its route disappears, so cleanupStaleLinkDownRoutes can't reach these rules and
// they accumulate indefinitely on the shared CI runner, slowing every iptables
// operation under the xtables lock.
//
// This is conservative and safe: a rule whose bridge interface is gone can never
// affect live traffic, and we never delete a rule whose interface still exists.
func sweepOrphanedTestIPTablesRules() {
	sweepOrphanedTestRulesInChain("", "FORWARD")
	sweepOrphanedTestRulesInChain("nat", "POSTROUTING")
}

func sweepOrphanedTestRulesInChain(table, chain string) {
	args := []string{}
	if table != "" {
		args = append(args, "-t", table)
	}
	args = append(args, "-S", chain)
	output, err := newTestIPTablesCommand(args...).Output()
	if err != nil {
		logTestNetworkErr(fmt.Sprintf("iptables -S %s/%s for orphan sweep", table, chain), err)
		return
	}

	// Collect rules whose bridge interface is gone. Cache bridge existence, but
	// retain duplicate rules so the sweep removes every copy.
	exists := make(map[string]bool)
	var orphanedRules [][]string
	for _, line := range strings.Split(string(output), "\n") {
		delArgs, comment, ok := parseIPTablesAppendRule(table, line)
		if !ok {
			continue
		}
		match := testRuleCommentPattern.FindStringSubmatch(comment)
		if match == nil {
			continue
		}
		bridge := match[1]

		alive, checked := exists[bridge]
		if !checked {
			alive = bridgeExists(bridge)
			exists[bridge] = alive
		}
		if alive {
			// Never delete a rule whose bridge interface still exists.
			continue
		}
		orphanedRules = append(orphanedRules, delArgs)
	}

	for _, delArgs := range orphanedRules {
		deleteIPTablesRuleWithRetry(delArgs)
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
		// The route may already be gone (linkdown cleanup, prior pass) — that is
		// not a real failure, so only surface unexpected errors.
		out, err := exec.Command("ip", "-4", "route", "del", subnetCIDR, "dev", bridgeName).CombinedOutput()
		if err != nil && !isNoSuchObjectErr(out) {
			logTestNetworkErr(fmt.Sprintf("ip route del %s dev %s: %s", subnetCIDR, bridgeName, strings.TrimSpace(string(out))), err)
		}
	}
	if bridgeName != "" {
		out, err := exec.Command("ip", "link", "delete", bridgeName, "type", "bridge").CombinedOutput()
		if err != nil && !isNoSuchObjectErr(out) {
			logTestNetworkErr(fmt.Sprintf("ip link delete %s: %s", bridgeName, strings.TrimSpace(string(out))), err)
		}
	}

	bridgeSuffix := strings.ToLower(bridgeName)
	deleteIPTablesRulesByComment("nat", "POSTROUTING", "hypeman-nat-"+bridgeSuffix)
	deleteIPTablesRulesByComment("", "FORWARD", "hypeman-fwd-out-"+bridgeSuffix)
	deleteIPTablesRulesByComment("", "FORWARD", "hypeman-fwd-in-"+bridgeSuffix)
}

// isNoSuchObjectErr reports whether an `ip` command output indicates the object
// (route/link) was already gone, which is an expected, benign outcome for
// idempotent cleanup.
func isNoSuchObjectErr(combinedOutput []byte) bool {
	out := strings.ToLower(string(combinedOutput))
	return strings.Contains(out, "cannot find") || strings.Contains(out, "no such")
}

func deleteIPTablesRulesByComment(table, chain, comment string) {
	if comment == "" {
		return
	}

	args := []string{}
	if table != "" {
		args = append(args, "-t", table)
	}
	args = append(args, "-S", chain)
	output, err := newTestIPTablesCommand(args...).Output()
	if err != nil {
		logTestNetworkErr(fmt.Sprintf("iptables -S %s/%s for comment %q", table, chain, comment), err)
		return
	}

	for _, line := range strings.Split(string(output), "\n") {
		delArgs, ruleComment, ok := parseIPTablesAppendRule(table, line)
		if !ok || ruleComment != comment {
			continue
		}
		deleteIPTablesRuleWithRetry(delArgs)
	}
}

func parseIPTablesAppendRule(table, line string) ([]string, string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "-A" {
		return nil, "", false
	}
	for i := range fields {
		fields[i] = strings.Trim(fields[i], `"'`)
	}

	var comment string
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "--comment" {
			comment = fields[i+1]
			break
		}
	}
	if comment == "" {
		return nil, "", false
	}

	fields[0] = "-D"
	args := make([]string, 0, len(fields)+2)
	if table != "" {
		args = append(args, "-t", table)
	}
	args = append(args, fields...)
	return args, comment, true
}

// deleteIPTablesRuleWithRetry runs an iptables `-D` delete, retrying a few times
// on transient lock contention (the failure mode under concurrent CI jobs).
func deleteIPTablesRuleWithRetry(delArgs []string) {
	var err error
	for attempt := 0; attempt < iptablesDeleteAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
		err = newTestIPTablesCommand(delArgs...).Run()
		if err == nil {
			return
		}
	}
	logTestNetworkErr(fmt.Sprintf("iptables %s", strings.Join(delArgs, " ")), err)
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
