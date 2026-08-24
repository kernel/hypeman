package hypervisor

import "time"

// VFIOTermGrace allows VFIO teardown to finish after SIGTERM before SIGKILL;
// SIGKILL during initialization can wedge the VF, while teardown takes 1-2s.
const VFIOTermGrace = 5 * time.Second
