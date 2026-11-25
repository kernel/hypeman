package hypervisor

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Match the kernel's process name as well as the control socket in the
// shim's -config argument. Other processes can mention that socket in argv.
func vzShimPIDsFromPS(out []byte, socketPath string) []int {
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		command, arg, ok := strings.Cut(line, " -config ")
		if !ok {
			continue
		}
		fields := strings.Fields(command)
		if len(fields) < 3 {
			continue
		}
		if name := fields[1]; name != "vz-shim" && !strings.HasPrefix(name, "vz-shim-") {
			continue
		}
		var config struct {
			ControlSocket string `json:"control_socket"`
		}
		if err := json.Unmarshal([]byte(arg), &config); err != nil || config.ControlSocket != socketPath {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}
