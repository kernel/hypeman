//go:build !darwin

package main

import "fmt"

func runDesktopAgent() error { return fmt.Errorf("desktop HTTP role is only supported on Darwin") }
