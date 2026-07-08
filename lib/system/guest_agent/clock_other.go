//go:build !linux

package main

import "errors"

func setRealtimeClock(int64) error {
	return errors.New("setting the realtime clock is only supported on linux")
}
