//go:build linux

package egressproxy

import (
	"fmt"
	"os/exec"
	"strings"
)

func applyEgressEnforcement(instanceID, tapDevice, gatewayIP string, proxyPort int) error {
	if instanceID == "" || tapDevice == "" || gatewayIP == "" || proxyPort <= 0 {
		return fmt.Errorf("invalid egress enforcement inputs")
	}

	comment80 := enforcementComment(instanceID, "80")
	comment443 := enforcementComment(instanceID, "443")

	// Clean old rules first so updates are idempotent (tap changes across restarts).
	_ = removeRuleByComment(comment80)
	_ = removeRuleByComment(comment443)

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
	comment80 := enforcementComment(instanceID, "80")
	comment443 := enforcementComment(instanceID, "443")
	_ = removeRuleByComment(comment80)
	_ = removeRuleByComment(comment443)
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

func removeRuleByComment(comment string) error {
	listCmd := exec.Command("iptables", "-L", "FORWARD", "--line-numbers", "-n")
	output, err := listCmd.Output()
	if err != nil {
		return err
	}

	var ruleNums []string
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, comment) {
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
	short := instanceID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("hypeman-egress-%s-%s", short, suffix)
}
