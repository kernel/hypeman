//go:build unix

package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// setupResizeHandler listens for SIGWINCH signals and sends resize messages over the WebSocket.
// Returns a cleanup function that should be deferred.
func setupResizeHandler(ws *websocket.Conn, wsMu *sync.Mutex) (cleanup func()) {
	sigwinch := make(chan os.Signal, 1)
	signal.Notify(sigwinch, syscall.SIGWINCH)

	go func() {
		for range sigwinch {
			cols, rows, _ := term.GetSize(int(os.Stdout.Fd()))
			if rows > 0 && cols > 0 {
				msg := fmt.Sprintf(`{"resize":{"rows":%d,"cols":%d}}`, rows, cols)
				wsMu.Lock()
				ws.WriteMessage(websocket.TextMessage, []byte(msg))
				wsMu.Unlock()
			}
		}
	}()

	return func() { signal.Stop(sigwinch) }
}
