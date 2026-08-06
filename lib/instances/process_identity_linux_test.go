//go:build linux

package instances

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHypervisorProcessExistsRejectsLivePIDWithoutSocketOwnership(t *testing.T) {
	t.Parallel()

	assert.False(t, HypervisorProcessExists(os.Getpid(), filepath.Join(t.TempDir(), "missing.sock")))
}
