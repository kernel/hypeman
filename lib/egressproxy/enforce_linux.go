//go:build linux

package egressproxy

import (
	"fmt"
	"os/exec"
	"strings"
	"unicode"
)

const (
	enforcementSuffixPort80  = "80"
	enforcementSuffixPort443 = "443"
	enforcementSuffixAllTCP  = "all-tcp"
)

func applyEgressEnforcement(instanceID, tapDevice, gatewayIP string, proxyPort int, blockAllTCPEgress bool) error {
	if instanceID == "" || tapDevice == "" || gatewayIP == "" || proxyPort <= 0 {
		return fmt.Errorf("invalid egress enforcement inputs")
	}

	comment80 := enforcementComment(instanceID, enforcementSuffixPort80)
	comment443 := enforcementComment(instanceID, enforcementSuffixPort443)
	commentAllTCP := enforcementComment(instanceID, enforcementSuffixAllTCP)

	// Clean old rules first so updates are idempotent across restarts and mode changes.
	_ = removeRuleByComment(comment80)
	_ = removeRuleByComment(comment443)
	_ = removeRuleByComment(commentAllTCP)

	if blockAllTCPEgress {
		if err := insertRejectAllTCPRule(tapDevice, gatewayIP, commentAllTCP); err != nil {
			return fmt.Errorf("insert all-tcp egress enforcement: %w", err)
		}
		return nil
	}

	if err := insertRejectRule(tapDevice, gatewayIP, 80, comment80); err != nil {
		return fmt.Errorf("insert port 80 egress enforcement: %w", err)
	}
	if err := insertRejectRule(tapDevice, gatewayIP, 443, comment443); err != nil {
		_ = removeRuleByComment(comment80)
		return fmt.Errorf("insert port 443 egress enforcement: %w", err)
	}

	return nil
}

func removeEgressEnforcement(instanceID string) error {
	if instanceID == "" {
		return nil
	}
	comment80 := enforcementComment(instanceID, enforcementSuffixPort80)
	comment443 := enforcementComment(instanceID, enforcementSuffixPort443)
	commentAllTCP := enforcementComment(instanceID, enforcementSuffixAllTCP)
	_ = removeRuleByComment(comment80)
	_ = removeRuleByComment(comment443)
	_ = removeRuleByComment(commentAllTCP)
	return nil
}

func insertRejectRule(tapDevice, gatewayIP string, port int, comment string) error {
	cmd := exec.Command(
		"iptables", "-I", "FORWARD", "1",
		"-i", tapDevice,
		"-p", "tcp",
		"--dport", fmt.Sprintf("%d", port),
		"!", "-d", gatewayIP,
		"-m", "comment", "--comment", comment,
		"-j", "REJECT",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables insert failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func insertRejectAllTCPRule(tapDevice, gatewayIP, comment string) error {
	cmd := exec.Command(
		"iptables", "-I", "FORWARD", "1",
		"-i", tapDevice,
		"-p", "tcp",
		"!", "-d", gatewayIP,
		"-m", "comment", "--comment", comment,
		"-j", "REJECT",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables insert failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeRuleByComment(comment string) error {
	listCmd := exec.Command("iptables", "-L", "FORWARD", "--line-numbers", "-n")
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
		delCmd := exec.Command("iptables", "-D", "FORWARD", ruleNums[i])
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
