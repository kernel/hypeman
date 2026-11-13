package hypervisor

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProcessEnvKeepsAllowlistOnly(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TMPDIR", "/var/tmp")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	env := ProcessEnv()

	assert.Contains(t, env, "PATH=/usr/bin:/bin")
	assert.Contains(t, env, "TMPDIR=/var/tmp")
	for _, entry := range env {
		assert.NotContains(t, entry, "JWT_SECRET")
		assert.NotContains(t, entry, "AWS_SECRET_ACCESS_KEY")
	}
}

func TestProcessEnvDefaultsPath(t *testing.T) {
	t.Setenv("PATH", "")
	os.Unsetenv("PATH")

	assert.Contains(t, ProcessEnv(), "PATH="+defaultProcessPath)
}
