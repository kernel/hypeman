//go:build linux

package main

func runPlatform(run func() error) error { return run() }
