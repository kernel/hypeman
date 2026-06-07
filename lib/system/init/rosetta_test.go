package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRosettaBinfmtRule(t *testing.T) {
	interp := "/run/rosetta/rosetta"
	rule := rosettaBinfmtRule(interp)

	want := ":rosetta:M::" +
		`\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00` + ":" +
		`\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff` + ":" +
		interp + ":OCF"

	assert.Equal(t, want, rule)
}

func TestRosettaBinfmtRuleFields(t *testing.T) {
	rule := rosettaBinfmtRule("/run/rosetta/rosetta")
	fields := strings.Split(rule, ":")

	// Leading colon yields an empty first field, then name/type/offset/magic/mask/interp/flags.
	assert.Equal(t, []string{"", "rosetta", "M", ""}, fields[:4])
	assert.Equal(t, "/run/rosetta/rosetta", fields[6])
	assert.Equal(t, "OCF", fields[7])
	assert.Contains(t, fields[7], "F", "fix-binary flag must be present so the interpreter fd survives chroot")
}
