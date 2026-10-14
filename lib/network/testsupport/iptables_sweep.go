// Package testsupport holds test-only helpers shared by the packages that bring
// up real hypeman networks in tests.
package testsupport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	iptablesWaitSeconds = "5"
	commandTimeout      = 30 * time.Second
	// maxBatchBytes caps one iptables-restore input. iptables 1.8 with the nft
	// backend fails a larger batch with "sendmsg() failed: Message too long", so
	// a large sweep is split into several batches, each committed on its own.
	maxBatchBytes = 16 << 10
)

// testBridgeName is the bridge name shape the test helpers generate: an hm, ha
// or hi prefix followed by hex digits. Production bridges come from config
// (default vmbr0) and never match.
const testBridgeName = `(?:hm|ha|hi)[0-9a-f]+`

// ruleCommentPattern matches the comments lib/network puts on hypeman's FORWARD
// and nat rules when the bridge is a test bridge.
var ruleCommentPattern = regexp.MustCompile(`^hypeman-(?:fwd-in|fwd-out|nat)-(` + testBridgeName + `)$`)

var testBridgePattern = regexp.MustCompile(`^` + testBridgeName + `$`)

// IsTestBridgeName reports whether name is a bridge the test helpers create.
func IsTestBridgeName(name string) bool {
	return testBridgePattern.MatchString(name)
}

// rule is a test-owned iptables rule parsed from `iptables -S` output.
type rule struct {
	// bridge is the bridge named by the rule comment.
	bridge string
	// deleteLine is the rule as an iptables-restore -D line.
	deleteLine string
}

var sweepOnce sync.Once

// SweepOrphanedRulesOnce runs SweepOrphanedRules at most once per process.
func SweepOrphanedRulesOnce() {
	sweepOnce.Do(SweepOrphanedRules)
}

// SweepOrphanedRules deletes test iptables rules whose bridge no longer exists
// from the FORWARD and nat POSTROUTING chains. Rules whose bridge exists are left
// alone, as are rules without a test-harness comment. Failures are logged, never
// returned, so a sweep problem cannot fail a test run.
func SweepOrphanedRules() {
	if runtime.GOOS != "linux" {
		return
	}
	sweepTable("filter", "FORWARD")
	sweepTable("nat", "POSTROUTING")
}

func sweepTable(table, chain string) {
	removed, err := sweepTableOnce(table, chain)
	if err != nil {
		// A rule can vanish mid-sweep when another test process cleans it up.
		// Rescan once; rules already deleted by the first pass are not listed.
		var more int
		more, err = sweepTableOnce(table, chain)
		removed += more
	}
	if err != nil {
		logErr(fmt.Sprintf("sweep orphaned %s/%s rules", table, chain), err)
		return
	}
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "hypeman-test-network: removed %d orphaned %s/%s rules\n", removed, table, chain)
	}
}

func sweepTableOnce(table, chain string) (int, error) {
	output, err := run(nil, "iptables", "-w", iptablesWaitSeconds, "-t", table, "-S", chain)
	if err != nil {
		return 0, fmt.Errorf("list rules: %w", err)
	}

	rules := parseRules(string(output))
	if len(rules) == 0 {
		return 0, nil
	}

	// Read links after the rules: a rule only exists once its bridge does, so a
	// bridge missing from this later listing really was removed.
	output, err = run(nil, "ip", "-o", "link", "show")
	if err != nil {
		return 0, fmt.Errorf("list links: %w", err)
	}

	orphans := orphanedRules(rules, parseLinkNames(string(output)))
	if len(orphans) == 0 {
		return 0, nil
	}

	removed := 0
	for _, batch := range batchRules(orphans, maxBatchBytes) {
		payload := restorePayload(table, batch)
		if _, err := run(strings.NewReader(payload), "iptables-restore", "-w", iptablesWaitSeconds, "--noflush"); err != nil {
			return removed, fmt.Errorf("delete rules from %s: %w", table, err)
		}
		removed += len(batch)
	}
	return removed, nil
}

// run executes a command with a bounded runtime and returns its stdout. Stderr
// is folded into the error so a failed batch is diagnosable from the log.
func run(stdin io.Reader, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	out, err := cmd.Output()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return out, err
}

func logErr(op string, err error) {
	fmt.Fprintf(os.Stderr, "hypeman-test-network: %s: %v\n", op, err)
}

// parseRules returns the test-owned rules in `iptables -S <chain>` output.
// Duplicate rules are kept so every copy is removed.
func parseRules(output string) []rule {
	var rules []rule
	for _, line := range strings.Split(output, "\n") {
		args := splitRuleArgs(line)
		if len(args) < 2 || args[0] != "-A" {
			continue
		}
		match := ruleCommentPattern.FindStringSubmatch(commentArg(args))
		if match == nil {
			continue
		}
		args[0] = "-D"
		rules = append(rules, rule{bridge: match[1], deleteLine: joinRestoreArgs(args)})
	}
	return rules
}

// orphanedRules returns the rules whose bridge is not in the live link set.
func orphanedRules(rules []rule, liveLinks map[string]bool) []rule {
	var orphans []rule
	for _, r := range rules {
		if !liveLinks[r.bridge] {
			orphans = append(orphans, r)
		}
	}
	return orphans
}

// batchRules splits rules into consecutive groups whose restore lines fit in
// maxBytes. A rule longer than maxBytes still gets a group of its own.
func batchRules(rules []rule, maxBytes int) [][]rule {
	var batches [][]rule
	var cur []rule
	size := 0
	for _, r := range rules {
		n := len(r.deleteLine) + 1
		if len(cur) > 0 && size+n > maxBytes {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
		cur = append(cur, r)
		size += n
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

// restorePayload builds iptables-restore input that deletes rules from one table
// in a single commit. It is meant to be fed to `iptables-restore --noflush`, so
// rules not listed are left untouched.
func restorePayload(table string, rules []rule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s\n", table)
	for _, r := range rules {
		b.WriteString(r.deleteLine)
		b.WriteByte('\n')
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// parseLinkNames returns the interface names in `ip -o link show` output.
func parseLinkNames(output string) map[string]bool {
	names := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasSuffix(fields[0], ":") {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		// Veth and VLAN peers are printed as name@peer.
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		names[name] = true
	}
	return names
}

func commentArg(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--comment" {
			return args[i+1]
		}
	}
	return ""
}

// splitRuleArgs splits one `iptables -S` line into arguments. Double- and
// single-quoted strings are kept whole and their quotes removed.
func splitRuleArgs(line string) []string {
	var args []string
	var cur strings.Builder
	var quote rune
	inArg := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inArg = true
		case r == ' ' || r == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}

// joinRestoreArgs joins arguments into one iptables-restore line, quoting any
// argument that contains whitespace.
func joinRestoreArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			quoted[i] = `"` + arg + `"`
		} else {
			quoted[i] = arg
		}
	}
	return strings.Join(quoted, " ")
}
