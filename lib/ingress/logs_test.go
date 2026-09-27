package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func forwardedLog(t *testing.T, line string) (map[string]any, string) {
	t.Helper()
	var output bytes.Buffer
	forwarder := CaddyLogForwarder{
		logger: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	forwarder.forwardLogLine(context.Background(), line)
	var record map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	return record, output.String()
}

func TestForwardLogLineAccessEntry(t *testing.T) {
	host := "session.example.test"
	entry := `{"level":"info","ts":1788907156.1,"logger":"http.log.access","msg":"message-secret","error":"error-secret","request":{"method":"GET","host":"session.example.test","uri":"/live/path-secret?token=query-secret","client_ip":"192.0.2.1","headers":{"Authorization":["Bearer header-secret"],"Cookie":["session=cookie-secret"]}},"status":502,"size":34,"bytes_read":12,"duration":0.125}`
	record, output := forwardedLog(t, entry)

	require.Equal(t, "GET", record["http_method"])
	require.Equal(t, float64(502), record["http_status"])
	require.Equal(t, float64(34), record["bytes_written"])
	require.Equal(t, float64(12), record["bytes_read"])
	require.Equal(t, 0.125, record["duration_seconds"])
	hostHash := sha256.Sum256([]byte(host))
	require.Equal(t, fmt.Sprintf("%x", hostHash), record["http_host_sha256"])
	for _, sensitive := range []string{host, "192.0.2.1", "path-secret", "query-secret", "header-secret", "cookie-secret", "message-secret", "error-secret"} {
		require.NotContains(t, output, sensitive)
	}
	require.NotContains(t, record, "http_path")
	require.NotContains(t, record, "client_ip")
}

func TestForwardLogLineBoundsMethodAndNonAccessFields(t *testing.T) {
	record, output := forwardedLog(t, `{"level":"info","logger":"http.log.access","msg":"handled request","request":{"method":"secret-method","host":""},"status":200}`)
	require.Equal(t, "OTHER", record["http_method"])
	require.NotContains(t, record, "http_host_sha256")
	require.NotContains(t, output, "secret-method")

	record, _ = forwardedLog(t, `{"level":"info","logger":"admin","msg":"config loaded","request":{"method":"GET","host":"session.example.test"},"status":200}`)
	require.Equal(t, "admin", record["caddy_logger"])
	require.NotContains(t, record, "http_status")
	require.NotContains(t, record, "http_host_sha256")
}

func TestForwardLogLineInvalidJSONDoesNotForwardRawLine(t *testing.T) {
	record, output := forwardedLog(t, `{"request":{"headers":{"Authorization":"secret"}`)
	require.Equal(t, "caddy: invalid JSON log entry", record["msg"])
	require.NotContains(t, output, "secret")
}
