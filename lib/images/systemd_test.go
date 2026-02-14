package images

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsSystemdImage(t *testing.T) {
	tests := []struct {
		name       string
		entrypoint []string
		cmd        []string
		expected   bool
	}{
		{
			name:       "empty entrypoint and cmd",
			entrypoint: nil,
			cmd:        nil,
			expected:   false,
		},
		{
			name:       "/sbin/init as cmd",
			entrypoint: nil,
			cmd:        []string{"/sbin/init"},
			expected:   true,
		},
		{
			name:       "/lib/systemd/systemd as cmd",
			entrypoint: nil,
			cmd:        []string{"/lib/systemd/systemd"},
			expected:   true,
		},
		{
			name:       "/usr/lib/systemd/systemd as cmd",
			entrypoint: nil,
			cmd:        []string{"/usr/lib/systemd/systemd"},
			expected:   true,
		},
		{
			name:       "path ending in /init should not match (too broad)",
			entrypoint: nil,
			cmd:        []string{"/usr/sbin/init"},
			expected:   false,
		},
		{
			name:       "regular command (nginx)",
			entrypoint: []string{"nginx"},
			cmd:        []string{"-g", "daemon off;"},
			expected:   false,
		},
		{
			name:       "regular command (python)",
			entrypoint: []string{"/usr/bin/python3"},
			cmd:        []string{"app.py"},
			expected:   false,
		},
		{
			name:       "entrypoint with systemd",
			entrypoint: []string{"/lib/systemd/systemd"},
			cmd:        nil,
			expected:   true,
		},
		{
			name:       "entrypoint with init",
			entrypoint: []string{"/sbin/init"},
			cmd:        nil,
			expected:   true,
		},
		{
			name:       "shell script named init should not match",
			entrypoint: nil,
			cmd:        []string{"./init"},
			expected:   false,
		},
		{
			name:       "bash command should not match",
			entrypoint: nil,
			cmd:        []string{"/bin/bash", "-c", "init"},
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsSystemdImage(tt.entrypoint, tt.cmd)
			assert.Equal(t, tt.expected, result)
		})
	}
}

