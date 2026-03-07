//go:build !linux

package main

import "github.com/kernel/hypeman/lib/paths"

func ensureHypervisorBinaries(_ *paths.Paths) (int, string, error) {
	return 0, "", nil
}
