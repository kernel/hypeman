//go:build windows

package cmd

import (
	"sync"

	"github.com/gorilla/websocket"
)

// setupResizeHandler is a no-op on Windows since SIGWINCH doesn't exist.
// Terminal resize events are not supported on native Windows.
// The initial terminal size is still sent in the exec request.
func setupResizeHandler(ws *websocket.Conn, wsMu *sync.Mutex) (cleanup func()) {
	return func() {}
}
