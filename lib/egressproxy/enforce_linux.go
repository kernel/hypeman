//go:build linux

package egressproxy

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"unicode"
)

const (
	enforcementSuffixPort80       = "80"
	enforcementSuffixPort443      = "443"
	enforcementSuffixAllTCP       = "all-tcp"
	enforcementSuffixHostTCP      = "host-tcp"
	enforcementSuffixHostDNS      = "host-dns"
	enforcementSuffixHostProxy    = "host-proxy"
	enforcementSuffixForwardSpoof = "forward-spoof"
	enforcementSuffixHostSpoof    = "host-spoof"
)

func applyEgressEnforcement(instanceID, sourceIP, tapDevice, gatewayIP string, proxyPort int, blockAllTCPEgress bool) error {
	if instanceID == "" || net.ParseIP(sourceIP).To4() == nil || tapDevice == "" || net.ParseIP(gatewayIP).To4() == nil || proxyPort <= 0 {
		return fmt.Errorf("invalid egress enforcement inputs")
	}

	removeEgressRules(instanceID)
	if err := insertAntiSpoofRule("FORWARD", sourceIP, tapDevice, enforcementComment(instanceID, enforcementSuffixForwardSpoof)); err != nil {
		return fmt.Errorf("insert forwarded anti-spoof enforcement: %w", err)
	}
	if err := insertAntiSpoofRule("INPUT", sourceIP, tapDevice, enforcementComment(instanceID, enforcementSuffixHostSpoof)); err != nil {
		removeEgressRules(instanceID)
		return fmt.Errorf("insert host anti-spoof enforcement: %w", err)
	}
	if blockAllTCPEgress {
		if err := insertRejectAllTCPRule(sourceIP, enforcementComment(instanceID, enforcementSuffixAllTCP)); err != nil {
			return fmt.Errorf("insert all-tcp egress enforcement: %w", err)
		}
		if err := insertRejectHostTCPRule(sourceIP, enforcementComment(instanceID, enforcementSuffixHostTCP)); err != nil {
			removeEgressRules(instanceID)
			return fmt.Errorf("insert host-tcp egress enforcement: %w", err)
		}
		if err := insertAcceptHostTCPRule(sourceIP, 53, enforcementComment(instanceID, enforcementSuffixHostDNS)); err != nil {
			removeEgressRules(instanceID)
			return fmt.Errorf("insert host DNS allowance: %w", err)
		}
		if err := insertAcceptHostTCPRule(sourceIP, proxyPort, enforcementComment(instanceID, enforcementSuffixHostProxy)); err != nil {
			removeEgressRules(instanceID)
			return fmt.Errorf("insert host proxy allowance: %w", err)
		}
		return nil
	}

	comment80 := enforcementComment(instanceID, enforcementSuffixPort80)
	if err := insertRejectRule(sourceIP, 80, comment80); err != nil {
		return fmt.Errorf("insert port 80 egress enforcement: %w", err)
	}
	if err := insertRejectRule(sourceIP, 443, enforcementComment(instanceID, enforcementSuffixPort443)); err != nil {
		removeEgressRules(instanceID)
		return fmt.Errorf("insert port 443 egress enforcement: %w", err)
	}
	return nil
}

func removeEgressEnforcement(instanceID string) error {
	if instanceID != "" {
		removeEgressRules(instanceID)
	}
	return nil
}

func removeEgressRules(instanceID string) {
	for _, rule := range []struct {
		chain  string
		suffix string
	}{
		{chain: "FORWARD", suffix: enforcementSuffixPort80},
		{chain: "FORWARD", suffix: enforcementSuffixPort443},
		{chain: "FORWARD", suffix: enforcementSuffixAllTCP},
		{chain: "INPUT", suffix: enforcementSuffixHostTCP},
		{chain: "INPUT", suffix: enforcementSuffixHostDNS},
		{chain: "INPUT", suffix: enforcementSuffixHostProxy},
		{chain: "FORWARD", suffix: enforcementSuffixForwardSpoof},
		{chain: "INPUT", suffix: enforcementSuffixHostSpoof},
	} {
		_ = removeRuleByComment(rule.chain, enforcementComment(instanceID, rule.suffix))
	}
}

func insertRejectRule(sourceIP string, port int, comment string) error {
	return insertIptablesRule(rejectRuleArgs(sourceIP, port, comment))
}

func insertRejectAllTCPRule(sourceIP, comment string) error {
	return insertIptablesRule(rejectAllTCPRuleArgs(sourceIP, comment))
}

func insertRejectHostTCPRule(sourceIP, comment string) error {
	return insertIptablesRule(rejectHostTCPRuleArgs(sourceIP, comment))
}

func insertAcceptHostTCPRule(sourceIP string, port int, comment string) error {
	return insertIptablesRule(acceptHostTCPRuleArgs(sourceIP, port, comment))
}

func insertAntiSpoofRule(chain, sourceIP, tapDevice, comment string) error {
	return insertIptablesRule(antiSpoofRuleArgs(chain, sourceIP, tapDevice, comment))
}

func insertIptablesRule(arguments []string) error {
	cmd := exec.Command("iptables", arguments...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables insert failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func rejectRuleArgs(sourceIP string, port int, comment string) []string {
	return []string{
		"-I", "FORWARD", "1",
		"-s", sourceIP,
		"-p", "tcp",
		"--dport", fmt.Sprintf("%d", port),
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", comment,
		"-j", "REJECT",
	}
}

func rejectAllTCPRuleArgs(sourceIP, comment string) []string {
	return []string{
		"-I", "FORWARD", "1",
		"-s", sourceIP,
		"-p", "tcp",
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", comment,
		"-j", "REJECT",
	}
}

func rejectHostTCPRuleArgs(sourceIP, comment string) []string {
	return []string{
		"-I", "INPUT", "1",
		"-s", sourceIP,
		"-p", "tcp",
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", comment,
		"-j", "REJECT",
	}
}

func acceptHostTCPRuleArgs(sourceIP string, port int, comment string) []string {
	return []string{
		"-I", "INPUT", "1",
		"-s", sourceIP,
		"-p", "tcp",
		"--dport", fmt.Sprintf("%d", port),
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT",
	}
}

func antiSpoofRuleArgs(chain, sourceIP, tapDevice, comment string) []string {
	return []string{
		"-I", chain, "1",
		"-m", "physdev", "--physdev-in", tapDevice,
		"!", "-s", sourceIP,
		"-m", "comment", "--comment", comment,
		"-j", "DROP",
	}
}

func removeRuleByComment(chain, comment string) error {
	listCmd := exec.Command("iptables", "-L", chain, "--line-numbers", "-n")
	output, err := listCmd.Output()
	if err != nil {
		return err
	}

	commentMarker := fmt.Sprintf("/* %s */", comment)
	var ruleNums []string
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, commentMarker) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ruleNums = append(ruleNums, fields[0])
	}

	for i := len(ruleNums) - 1; i >= 0; i-- {
		delCmd := exec.Command("iptables", "-D", chain, ruleNums[i])
		_ = delCmd.Run()
	}
	return nil
}

func enforcementComment(instanceID, suffix string) string {
	safeID := sanitizeInstanceIDForComment(instanceID)
	return fmt.Sprintf("hypeman-egress-%s-%s", safeID, suffix)
}

func sanitizeInstanceIDForComment(instanceID string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, strings.TrimSpace(instanceID))
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}
