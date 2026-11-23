//go:build !darwin

package main

// deliverInstanceEnv is macOS only: hypeman-init passes env to Linux workloads.
func deliverInstanceEnv() {}
