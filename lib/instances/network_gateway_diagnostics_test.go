//go:build linux

package instances

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func logGatewayDiagnostics(ctx context.Context, t *testing.T, inst *Instance, allocIP, gateway, tapDevice string, port int) {
	t.Helper()

	instanceID := ""
	if inst != nil {
		instanceID = inst.Id
		guestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		guestCmd := fmt.Sprintf("set -x; ip addr; ip route; ip neigh; ping -c 1 -W 2 %[1]s || true; nc -vz -w 2 %[1]s %[2]d || true", gateway, port)
		output, exitCode, err := execCommand(guestCtx, inst, "sh", "-lc", guestCmd)
		cancel()
		t.Logf("guest gateway diagnostics exit=%d err=%v\n%s", exitCode, err, output)
	}

	for _, args := range [][]string{
		{"ip", "addr"},
		{"ip", "route"},
		{"bridge", "link"},
		{"ss", "-ltnp"},
		{"iptables", "-L", "INPUT", "-n", "-v", "--line-numbers"},
		{"iptables", "-L", "OUTPUT", "-n", "-v", "--line-numbers"},
		{"iptables", "-L", "FORWARD", "-n", "-v", "--line-numbers"},
	} {
		cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		cancel()
		t.Logf("host diagnostics %s err=%v\n%s", strings.Join(args, " "), err, string(out))
	}

	t.Logf("gateway diagnostics summary instance=%s ip=%s gateway=%s tap=%s port=%d", instanceID, allocIP, gateway, tapDevice, port)
}

func gatewayPortFromURL(rawURL string) (int, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(port)
}
