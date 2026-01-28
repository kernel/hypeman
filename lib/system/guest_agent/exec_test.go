package main

import (
	"os"
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
			// Get base environment's TERM value before test
			baseTERM := ""
			for _, e := range os.Environ() {
				if strings.HasPrefix(e, "TERM=") {
					baseTERM = e
					break
				}
			}

			env := server.buildEnv(tt.envMap, tt.tty)

			// Collect all TERM entries
			termEntries := []string{}
			for _, e := range env {
				if strings.HasPrefix(e, "TERM=") {
					termEntries = append(termEntries, e)
				}
			}

			if tt.wantTERM == "" {
				// For non-TTY without user-provided TERM, verify behavior:
				// If os.Environ() had TERM, it should be preserved
				// If os.Environ() had no TERM, none should be added
				if baseTERM != "" {
					found := false
					for _, entry := range termEntries {
						if entry == baseTERM {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("Expected base environment TERM=%q to be preserved, but got: %v", baseTERM, termEntries)
					}
				}
				// Don't fail if buildEnv doesn't add TERM (expected behavior)
			} else {
				// Verify expected TERM is present
				found := false
				for _, entry := range termEntries {
					if entry == tt.wantTERM {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find %q in environment but didn't. Found TERM entries: %v", tt.wantTERM, termEntries)
				}
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
				if !found {
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

	// Count TERM entries and verify only one exists with the custom value
	termEntries := []string{}
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			termEntries = append(termEntries, e)
		}
	}

	if len(termEntries) == 0 {
		t.Error("Expected TERM to be present in environment")
	}

	// Verify only TERM=custom exists (no duplicates)
	foundCustom := false
	foundLinux := false
	for _, entry := range termEntries {
		if entry == "TERM=custom" {
			foundCustom = true
		}
		if entry == "TERM=linux" {
			foundLinux = true
		}
	}

	if !foundCustom {
		t.Errorf("Expected TERM=custom to be present, but found: %v", termEntries)
	}

	if foundLinux {
		t.Errorf("Expected TERM=linux NOT to be present when user provides custom TERM, but found: %v", termEntries)
	}
}
