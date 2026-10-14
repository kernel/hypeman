package testsupport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuleCommentPattern(t *testing.T) {
	accepted := []struct {
		comment string
		bridge  string
	}{
		{"hypeman-fwd-in-hm00c0a001", "hm00c0a001"},
		{"hypeman-fwd-out-ha00c0a001", "ha00c0a001"},
		{"hypeman-nat-hi00c0a001", "hi00c0a001"},
		{"hypeman-fwd-in-hm1", "hm1"},
	}
	for _, tc := range accepted {
		match := ruleCommentPattern.FindStringSubmatch(tc.comment)
		require.NotNil(t, match, tc.comment)
		assert.Equal(t, tc.bridge, match[1], tc.comment)
	}

	rejected := []string{
		"hypeman-fwd-in-vmbr0",
		"hypeman-fwd-out-vmbr0",
		"hypeman-nat-vmbr0",
		"hypeman-fwd-in-hm",
		"hypeman-fwd-in-hmxyz",
		"hypeman-fwd-in-hm00c0a001-extra",
		"hypeman-fwd-in-xhm00c0a001",
		"hypeman-fwd-in-",
		"hypeman-fwd-in-hm00c0a001 ",
		"hypeman-other-hm00c0a001",
		"hypeman-fwd-in",
		"kube-proxy",
		"",
	}
	for _, comment := range rejected {
		assert.Nil(t, ruleCommentPattern.FindStringSubmatch(comment), comment)
	}
}

const sampleForwardOutput = `-P FORWARD DROP
-N DOCKER-USER
-A FORWARD -i ha00c0a001 -o vmbr0 -m conntrack --ctstate NEW,ESTABLISHED,RELATED -m comment --comment "hypeman-fwd-out-ha00c0a001" -j ACCEPT
-A FORWARD -i vmbr0 -o ha00c0a001 -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment hypeman-fwd-in-ha00c0a001 -j ACCEPT
-A FORWARD -i ha00c0a001 -o vmbr0 -m conntrack --ctstate NEW,ESTABLISHED,RELATED -m comment --comment "hypeman-fwd-out-ha00c0a001" -j ACCEPT
-A FORWARD -i hm00c0a002 -o vmbr0 -m comment --comment "hypeman-fwd-out-hm00c0a002" -j ACCEPT
-A FORWARD -o vmbr0 -m comment --comment "hypeman-fwd-in-vmbr0" -j ACCEPT
-A FORWARD -m comment --comment "kube-proxy" -j ACCEPT
`

func TestParseRulesKeepsOnlyTestRulesAndDuplicates(t *testing.T) {
	rules := parseRules(sampleForwardOutput)

	require.Len(t, rules, 4)
	assert.Equal(t, "ha00c0a001", rules[0].bridge)
	assert.Equal(t, "ha00c0a001", rules[1].bridge)
	assert.Equal(t, "ha00c0a001", rules[2].bridge)
	assert.Equal(t, "hm00c0a002", rules[3].bridge)
}

func TestRestorePayloadFromSaveOutput(t *testing.T) {
	rules := parseRules(sampleForwardOutput)
	orphans := orphanedRules(rules, map[string]bool{"vmbr0": true, "hm00c0a002": true})

	require.Len(t, orphans, 3)
	assert.Equal(t, `*filter
-D FORWARD -i ha00c0a001 -o vmbr0 -m conntrack --ctstate NEW,ESTABLISHED,RELATED -m comment --comment hypeman-fwd-out-ha00c0a001 -j ACCEPT
-D FORWARD -i vmbr0 -o ha00c0a001 -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment hypeman-fwd-in-ha00c0a001 -j ACCEPT
-D FORWARD -i ha00c0a001 -o vmbr0 -m conntrack --ctstate NEW,ESTABLISHED,RELATED -m comment --comment hypeman-fwd-out-ha00c0a001 -j ACCEPT
COMMIT
`, restorePayload("filter", orphans))
}

func TestRestorePayloadNATTable(t *testing.T) {
	const natOutput = `-P POSTROUTING ACCEPT
-A POSTROUTING -s 10.200.1.0/24 ! -d 10.200.1.0/24 -m comment --comment "hypeman-nat-ha00c0a001" -j MASQUERADE
-A POSTROUTING -s 192.168.0.0/16 -j MASQUERADE
`
	orphans := orphanedRules(parseRules(natOutput), map[string]bool{})

	require.Len(t, orphans, 1)
	assert.Equal(t, `*nat
-D POSTROUTING -s 10.200.1.0/24 ! -d 10.200.1.0/24 -m comment --comment hypeman-nat-ha00c0a001 -j MASQUERADE
COMMIT
`, restorePayload("nat", orphans))
}

func TestOrphanedRulesSkipsLiveBridges(t *testing.T) {
	rules := parseRules(sampleForwardOutput)

	assert.Empty(t, orphanedRules(rules, map[string]bool{"ha00c0a001": true, "hm00c0a002": true}))
	assert.Len(t, orphanedRules(rules, map[string]bool{}), 4)
}

func TestSplitRuleArgsQuotes(t *testing.T) {
	assert.Equal(t,
		[]string{"-A", "FORWARD", "-m", "comment", "--comment", "a b", "-j", "ACCEPT"},
		splitRuleArgs(`-A FORWARD -m comment --comment "a b" -j ACCEPT`))
	assert.Equal(t,
		[]string{"-A", "FORWARD", "-m", "comment", "--comment", "hypeman-fwd-in-hm1", "-j", "ACCEPT"},
		splitRuleArgs(`-A FORWARD -m comment --comment 'hypeman-fwd-in-hm1' -j ACCEPT`))
	assert.Empty(t, splitRuleArgs("   "))
}

func TestJoinRestoreArgsQuotesWhitespace(t *testing.T) {
	assert.Equal(t,
		`-D FORWARD -m comment --comment "a b" -j ACCEPT`,
		joinRestoreArgs([]string{"-D", "FORWARD", "-m", "comment", "--comment", "a b", "-j", "ACCEPT"}))
}

func TestParseLinkNames(t *testing.T) {
	const linkOutput = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP mode DEFAULT group default qlen 1000\    link/ether 52:54:00:12:34:56 brd ff:ff:ff:ff:ff:ff
5: vmbr0@eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue master vmbr0 state UP\    link/ether 52:54:00:12:34:56 brd ff:ff:ff:ff:ff:ff
7: ha00c0a001: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP\    link/ether 02:00:00:00:00:01 brd ff:ff:ff:ff:ff:ff
`
	assert.Equal(t, map[string]bool{
		"lo":         true,
		"eth0":       true,
		"vmbr0":      true,
		"ha00c0a001": true,
	}, parseLinkNames(linkOutput))
}

func TestBatchRulesKeepsEveryRuleUnderLimit(t *testing.T) {
	var rules []rule
	for i := 0; i < 1000; i++ {
		rules = append(rules, rule{bridge: "ha00c0a001", deleteLine: "-D FORWARD -i ha00c0a001 -o vmbr0 -m comment --comment hypeman-fwd-out-ha00c0a001 -j ACCEPT"})
	}

	batches := batchRules(rules, 1024)

	require.Greater(t, len(batches), 1)
	total := 0
	for _, batch := range batches {
		total += len(batch)
		size := 0
		for _, r := range batch {
			size += len(r.deleteLine) + 1
		}
		assert.LessOrEqual(t, size, 1024)
	}
	assert.Equal(t, len(rules), total)
}

func TestBatchRulesOversizedRuleGetsOwnBatch(t *testing.T) {
	rules := []rule{{deleteLine: "short"}, {deleteLine: "this line is longer than the limit"}, {deleteLine: "short"}}

	batches := batchRules(rules, 10)

	require.Len(t, batches, 3)
	assert.Equal(t, "this line is longer than the limit", batches[1][0].deleteLine)
}

func TestIsTestBridgeName(t *testing.T) {
	for _, name := range []string{"hm00c0a001", "ha00c0a001", "hi00c0a001"} {
		assert.True(t, IsTestBridgeName(name), name)
	}
	for _, name := range []string{"vmbr0", "eth0", "docker0", "hm", "ha", "hi", "hmxyz", "hm00c0a001.1", "xhm00c0a001", ""} {
		assert.False(t, IsTestBridgeName(name), name)
	}
}
