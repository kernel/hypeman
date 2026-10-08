package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildEnv(t *testing.T) {
	s := &guestServer{}

	t.Run("TTY session gets TERM default", func(t *testing.T) {
		env := s.buildEnv(nil, true)
		assert.Contains(t, env, "TERM=xterm-256color")
	})

	t.Run("non-TTY session does not add xterm-256color", func(t *testing.T) {
		t.Setenv("TERM", "guest-agent-test")
		env := s.buildEnv(nil, false)
		assert.Contains(t, env, "TERM=guest-agent-test", "non-TTY preserves the inherited TERM")
		assert.NotContains(t, env, "TERM=xterm-256color", "non-TTY should not add xterm-256color default")
	})

	t.Run("user TERM overrides default", func(t *testing.T) {
		env := s.buildEnv(map[string]string{"TERM": "dumb"}, true)
		termCount := 0
		for _, e := range env {
			if strings.HasPrefix(e, "TERM=") {
				assert.Equal(t, "TERM=dumb", e)
				termCount++
			}
		}
		assert.Equal(t, 1, termCount, "should have exactly one TERM")
	})
}
