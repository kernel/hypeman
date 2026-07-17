package uffdpager

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var pagerVersion string

func Version() string {
	return strings.TrimSpace(pagerVersion)
}
