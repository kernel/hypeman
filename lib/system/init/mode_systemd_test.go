package main

import (
	"testing"

	"al.essio.dev/pkg/shellescape"
	"github.com/stretchr/testify/assert"
)

func TestBuildEnvFileContent(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		contains []string
	}{
		{
			name: "simple env vars",
			env: map[string]string{
				"FOO": "bar",
				"BAZ": "qux",
			},
			contains: []string{
				"FOO=bar\n",
				"BAZ=qux\n",
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n",
				"HOME=/root\n",
			},
		},
		{
			name: "env var with spaces uses single quotes",
			env: map[string]string{
				"MESSAGE": "hello world",
			},
			contains: []string{
				"MESSAGE='hello world'\n",
			},
		},
		{
			name: "env var with double quotes",
			env: map[string]string{
				"QUOTED": `say "hello"`,
			},
			contains: []string{
				// shellescape uses single quotes, so double quotes don't need escaping
				`QUOTED='say "hello"'` + "\n",
			},
		},
		{
			name: "env var with dollar sign",
			env: map[string]string{
				"VAR": "$HOME/path",
			},
			contains: []string{
				// Inside single quotes, $ is literal
				"VAR='$HOME/path'\n",
			},
		},
		{
			name: "custom PATH overrides default",
			env: map[string]string{
				"PATH": "/custom/path",
			},
			contains: []string{
				"PATH=/custom/path\n",
			},
		},
		{
			name: "custom HOME overrides default",
			env: map[string]string{
				"HOME": "/home/user",
			},
			contains: []string{
				"HOME=/home/user\n",
			},
		},
		{
			name: "empty env gets defaults",
			env:  map[string]string{},
			contains: []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n",
				"HOME=/root\n",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := buildEnvFileContent(tc.env)
			for _, expected := range tc.contains {
				assert.Contains(t, result, expected)
			}
		})
	}

	// Test that custom PATH doesn't get default PATH added
	t.Run("custom PATH excludes default", func(t *testing.T) {
		result := buildEnvFileContent(map[string]string{"PATH": "/custom"})
		assert.Contains(t, result, "PATH=/custom\n")
		assert.NotContains(t, result, "/usr/local/sbin")
	})

	// Test that custom HOME doesn't get default HOME added
	t.Run("custom HOME excludes default", func(t *testing.T) {
		result := buildEnvFileContent(map[string]string{"HOME": "/home/user"})
		assert.Contains(t, result, "HOME=/home/user\n")
		// Count occurrences of HOME=
		count := 0
		for i := 0; i < len(result)-5; i++ {
			if result[i:i+5] == "HOME=" {
				count++
			}
		}
		assert.Equal(t, 1, count, "HOME should appear exactly once")
	})
}

// TestShellescape verifies shellescape.Quote behavior for documentation
func TestShellescape(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple value unchanged",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "''",
		},
		{
			name:     "spaces get single quoted",
			input:    "hello world",
			expected: "'hello world'",
		},
		{
			name:     "double quotes safe in single quotes",
			input:    `say "hello"`,
			expected: `'say "hello"'`,
		},
		{
			name:     "single quote gets escaped",
			input:    "it's fine",
			expected: "'it'\"'\"'s fine'",
		},
		{
			name:     "dollar sign safe in single quotes",
			input:    "$HOME/file",
			expected: "'$HOME/file'",
		},
		{
			name:     "backtick safe in single quotes",
			input:    "`command`",
			expected: "'`command`'",
		},
		{
			name:     "newline gets quoted",
			input:    "line1\nline2",
			expected: "'line1\nline2'",
		},
		{
			name:     "path unchanged",
			input:    "/usr/local/bin",
			expected: "/usr/local/bin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := shellescape.Quote(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}
