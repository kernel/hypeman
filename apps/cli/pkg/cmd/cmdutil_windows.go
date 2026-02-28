//go:build windows

package cmd

import (
	"errors"
	"os"
)

// createSocketPair is not supported on Windows, so we return an error
// which causes createPagerFiles to fall back to using pipes.
func createSocketPair() (*os.File, *os.File, bool, error) {
	return nil, nil, false, errors.New("socket pairs not supported on Windows")
}
