// Package lib provides manually-maintained functionality that extends the auto-generated SDK.
package lib

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"
)

// WsConn abstracts a WebSocket connection for testing.
type WsConn interface {
	WriteMessage(messageType int, data []byte) error
	ReadMessage() (messageType int, p []byte, err error)
	Close() error
}

// WsDialer abstracts WebSocket connection creation for testing.
type WsDialer interface {
	DialContext(ctx context.Context, url string, headers http.Header) (WsConn, *http.Response, error)
}

// DefaultDialer uses gorilla/websocket for real WebSocket connections.
type DefaultDialer struct{}

// DialContext connects to a WebSocket server.
func (d *DefaultDialer) DialContext(ctx context.Context, url string, headers http.Header) (WsConn, *http.Response, error) {
	dialer := websocket.Dialer{}
	conn, resp, err := dialer.DialContext(ctx, url, headers)
	if err != nil {
		return nil, resp, err
	}
	return conn, resp, nil
}

// Ensure gorilla websocket.Conn implements WsConn
var _ WsConn = (*websocket.Conn)(nil)

