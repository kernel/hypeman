//go:build linux

package instances

import (
	"fmt"
	"os/exec"
	"strings"
)

func hostNetworkDiagnostics(tapDevice string) string {
	commands := [][]string{
		{"iptables", "-S", "INPUT"},
		{"iptables", "-S", "FORWARD"},
		{"iptables", "-vnL", "INPUT", "--line-numbers"},
		{"iptables", "-vnL", "FORWARD", "--line-numbers"},
	}

	var out strings.Builder
	for _, args := range commands {
		cmdOut, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		out.WriteString("$ ")
		out.WriteString(strings.Join(args, " "))
		out.WriteByte('\n')
		if err != nil {
			out.WriteString(fmt.Sprintf("error: %v\n", err))
		}
		out.Write(cmdOut)
		if len(cmdOut) == 0 || cmdOut[len(cmdOut)-1] != '\n' {
			out.WriteByte('\n')
		}
	}

	if tapDevice != "" {
		cmdOut, err := exec.Command("bridge", "link", "show", "dev", tapDevice).CombinedOutput()
		out.WriteString("$ bridge link show dev ")
		out.WriteString(tapDevice)
		out.WriteByte('\n')
		if err != nil {
			out.WriteString(fmt.Sprintf("error: %v\n", err))
		}
		out.Write(cmdOut)
		if len(cmdOut) == 0 || cmdOut[len(cmdOut)-1] != '\n' {
			out.WriteByte('\n')
		}
	}

	return out.String()
}
