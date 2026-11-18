package hypervisor

import "os"

// processEnvAllowlist is the set of variables passed from hypeman to the
// VMM and helper processes it launches. Hypeman's own environment carries
// its configuration, including credentials, which those processes don't need.
var processEnvAllowlist = []string{
	"PATH",
	"HOME",
	"TMPDIR",
	"LANG",
	"LC_ALL",
	"TZ",
}

// defaultProcessPath is used when hypeman itself runs without PATH. QEMU's
// exec: migration runs through /bin/sh, which needs PATH to find cat.
const defaultProcessPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// ProcessEnv returns the environment for a VMM or helper process.
func ProcessEnv() []string {
	env := make([]string, 0, len(processEnvAllowlist))
	for _, key := range processEnvAllowlist {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		} else if key == "PATH" {
			env = append(env, "PATH="+defaultProcessPath)
		}
	}
	return env
}
