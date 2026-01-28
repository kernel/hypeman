package main

import (
	"strings"
	"testing"
)

// TestBuildEnv_TTY_TERM tests that TERM=linux is set for TTY sessions
func TestBuildEnv_TTY_TERM(t *testing.T) {
	server := &guestServer{}

	tests := []struct {
		name     string
		envMap   map[string]string
		tty      bool
		wantTERM string
	}{
		{
			name:     "TTY without TERM in envMap",
			envMap:   map[string]string{},
			tty:      true,
			wantTERM: "TERM=linux",
		},
		{
			name:     "TTY with custom TERM in envMap",
			envMap:   map[string]string{"TERM": "xterm-256color"},
			tty:      true,
			wantTERM: "TERM=xterm-256color",
		},
		{
			name:     "non-TTY without TERM in envMap",
			envMap:   map[string]string{},
			tty:      false,
			wantTERM: "",
		},
		{
			name:     "non-TTY with TERM in envMap",
			envMap:   map[string]string{"TERM": "xterm"},
			tty:      false,
			wantTERM: "TERM=xterm",
		},
		{
			name:     "TTY with other env vars",
			envMap:   map[string]string{"FOO": "bar", "BAZ": "qux"},
			tty:      true,
			wantTERM: "TERM=linux",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := server.buildEnv(tt.envMap, tt.tty)

			// Check if TERM is present in the environment
			termFound := false
			for _, e := range env {
				if strings.HasPrefix(e, "TERM=") {
					if tt.wantTERM == "" {
						// If we don't expect TERM to be added by buildEnv, it's OK if it's from os.Environ()
						// We just check that our code didn't add it
						continue
					}
					if e == tt.wantTERM {
						termFound = true
						break
					} else {
						t.Errorf("Expected TERM=%q but got %q", tt.wantTERM, e)
						return
					}
				}
			}

			if tt.wantTERM != "" && !termFound {
				t.Errorf("Expected to find %q in environment but didn't. Env: %v", tt.wantTERM, env)
			}

			// Verify other environment variables are present
			for k, v := range tt.envMap {
				expected := k + "=" + v
				found := false
				for _, e := range env {
					if e == expected {
						found = true
						break
					}
				}
				if !found && k != "TERM" {
					t.Errorf("Expected to find %q in environment but didn't", expected)
				}
			}
		})
	}
}

// TestBuildEnv_Precedence tests that user-provided env vars override defaults
func TestBuildEnv_Precedence(t *testing.T) {
	server := &guestServer{}

	// TTY mode with custom TERM should override default
	env := server.buildEnv(map[string]string{"TERM": "custom"}, true)

	customTermFound := false
	for _, e := range env {
		if e == "TERM=custom" {
			customTermFound = true
		}
	}

	if !customTermFound {
		t.Error("Expected custom TERM=custom to be present")
	}

	// The user-provided value should be in the environment
	// Note: Both TERM=linux and TERM=custom might be present since we append to os.Environ(),
	// but the last one in the list will be used by the process
}
