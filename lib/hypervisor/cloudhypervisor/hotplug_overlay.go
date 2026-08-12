package cloudhypervisor

import (
	"os"
	"strconv"
	"strings"
)

const (
	experimentalHotplugOverlayEnv = "HYPEMAN_EXPERIMENTAL_CH_HOTPLUG_OVERLAY"
	experimentalMaxVCPUsEnv       = "HYPEMAN_EXPERIMENTAL_CH_MAX_VCPUS"
)

func ExperimentalMaxVCPUs() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(experimentalMaxVCPUsEnv)))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func ExperimentalHotplugOverlayEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(experimentalHotplugOverlayEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
