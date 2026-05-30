//go:build !linux

package main

type resumeNetworkController struct{}

func startResumeNetworkWatcher(_ *guestServer) {}
