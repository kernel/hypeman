//go:build windows

package main

import "os"

func setFileOwnership(string, uint32, uint32) error { return nil }
func fileOwnership(os.FileInfo) (uint32, uint32)    { return 0, 0 }
