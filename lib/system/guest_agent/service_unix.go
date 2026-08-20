//go:build !windows

package main

func runPlatform(run func() error) error { return run() }
