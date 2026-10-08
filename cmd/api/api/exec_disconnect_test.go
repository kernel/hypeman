package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestExecWebsocketDisconnectCancelsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			readDone <- err
			return
		}
		defer conn.Close()
		wrapped := &wsReadWriter{ws: conn, ctx: ctx, cancel: cancel}
		_, err = wrapped.Read(make([]byte, 1))
		readDone <- err
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("WebSocket disconnect did not cancel exec session")
	}
	select {
	case err := <-readDone:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("WebSocket reader did not exit")
	}
}
