//go:build linux

package egressproxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectRuleArgsMatchSourceAddress(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		"-I", "FORWARD", "1",
		"-s", "10.102.1.2",
		"-p", "tcp",
		"--dport", "443",
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", "hypeman-egress-instance-443",
		"-j", "REJECT",
	}, rejectRuleArgs("10.102.1.2", 443, "hypeman-egress-instance-443"))

	require.Equal(t, []string{
		"-I", "FORWARD", "1",
		"-s", "10.102.1.2",
		"-p", "tcp",
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", "hypeman-egress-instance-all-tcp",
		"-j", "REJECT",
	}, rejectAllTCPRuleArgs("10.102.1.2", "hypeman-egress-instance-all-tcp"))

	require.Equal(t, []string{
		"-I", "INPUT", "1",
		"-s", "10.102.1.2",
		"-p", "tcp",
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", "hypeman-egress-instance-host-tcp",
		"-j", "REJECT",
	}, rejectHostTCPRuleArgs("10.102.1.2", "hypeman-egress-instance-host-tcp"))

	require.Equal(t, []string{
		"-I", "INPUT", "1",
		"-s", "10.102.1.2",
		"-p", "tcp",
		"--dport", "18080",
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", "hypeman-egress-instance-host-proxy",
		"-j", "ACCEPT",
	}, acceptHostTCPRuleArgs("10.102.1.2", 18080, "hypeman-egress-instance-host-proxy"))

	require.Equal(t, []string{
		"-I", "FORWARD", "1",
		"-m", "physdev", "--physdev-in", "hype-instance",
		"!", "-s", "10.102.1.2",
		"-m", "comment", "--comment", "hypeman-egress-instance-forward-spoof",
		"-j", "DROP",
	}, antiSpoofRuleArgs("FORWARD", "10.102.1.2", "hype-instance", "hypeman-egress-instance-forward-spoof"))
}
