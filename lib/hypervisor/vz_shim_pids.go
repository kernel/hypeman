package hypervisor

import (
	"encoding/json"
	"strconv"
	"strings"
)

// vzShimPIDsFromPS returns the PIDs in `ps -o pid=,command=` output whose
// vz-shim configuration names socketPath as its control socket.
func vzShimPIDsFromPS(out []byte, socketPath string) []int {
	quoted, _ := json.Marshal(socketPath) // a string always marshals
	marker := `"control_socket":` + string(quoted)
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(line, " -config ") || !strings.Contains(line, marker) {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
