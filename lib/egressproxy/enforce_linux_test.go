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
		"-m", "comment", "--comment", "hypeman-egress-instance-443",
		"-j", "REJECT",
	}, rejectRuleArgs("10.102.1.2", 443, "hypeman-egress-instance-443"))

	require.Equal(t, []string{
		"-I", "FORWARD", "1",
		"-s", "10.102.1.2",
		"-p", "tcp",
		"-m", "comment", "--comment", "hypeman-egress-instance-all-tcp",
		"-j", "REJECT",
	}, rejectAllTCPRuleArgs("10.102.1.2", "hypeman-egress-instance-all-tcp"))
}
