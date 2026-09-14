package ingress

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureHandler records the attributes of every log record it handles.
type captureHandler struct {
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func (h *captureHandler) attrs(t *testing.T) map[string]any {
	t.Helper()
	require.Len(t, h.records, 1)
	attrs := map[string]any{}
	h.records[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	return attrs
}

func newCaptureForwarder() (*CaddyLogForwarder, *captureHandler) {
	h := &captureHandler{}
	return &CaddyLogForwarder{logger: slog.New(h)}, h
}

func TestForwardLogLineAccessEntry(t *testing.T) {
	f, h := newCaptureForwarder()

	f.forwardLogLine(context.Background(), `{"level":"info","ts":1788907156.1030767,"logger":"http.log.access","msg":"handled request","request":{"remote_ip":"157.245.71.106","client_ip":"157.245.71.106","proto":"HTTP/1.1","method":"GET","host":"abc123.prod-iad-hypeman-4.kernel.sh","uri":"/json/version","headers":{"Cookie":["session=secret"]}},"bytes_read":12,"duration":0.00001989,"size":34,"status":502}`)

	attrs := h.attrs(t)
	assert.Equal(t, "GET", attrs["http_method"])
	assert.Equal(t, "abc123.prod-iad-hypeman-4.kernel.sh", attrs["http_host"])
	assert.Equal(t, "/json/version", attrs["http_path"])
	assert.Equal(t, "HTTP/1.1", attrs["http_proto"])
	assert.Equal(t, "157.245.71.106", attrs["client_ip"])
	assert.Equal(t, int64(502), attrs["http_status"])
	assert.Equal(t, int64(34), attrs["bytes_written"])
	assert.Equal(t, int64(12), attrs["bytes_read"])
	assert.InDelta(t, 0.00001989, attrs["duration_seconds"], 1e-12)
}

func TestForwardLogLineDropsQueryAndHeaders(t *testing.T) {
	f, h := newCaptureForwarder()

	f.forwardLogLine(context.Background(), `{"level":"info","ts":1788907156.1,"logger":"http.log.access","msg":"handled request","request":{"method":"GET","host":"abc.kernel.sh","uri":"/live?token=super-secret","headers":{"Authorization":["Bearer super-secret"]}},"status":200}`)

	attrs := h.attrs(t)
	assert.Equal(t, "/live", attrs["http_path"])
	for k, v := range attrs {
		str, ok := v.(string)
		if !ok {
			continue
		}
		assert.NotContains(t, str, "super-secret", "attribute %q leaked a credential", k)
	}
}

func TestForwardLogLineNonAccessEntry(t *testing.T) {
	f, h := newCaptureForwarder()

	f.forwardLogLine(context.Background(), `{"level":"info","ts":1788907156.1,"logger":"admin","msg":"config loaded"}`)

	attrs := h.attrs(t)
	assert.NotContains(t, attrs, "http_status")
	assert.NotContains(t, attrs, "http_path")
	assert.Equal(t, "admin", attrs["caddy_logger"])
}

func TestRequestPath(t *testing.T) {
	assert.Equal(t, "/json/version", requestPath("/json/version"))
	assert.Equal(t, "/live", requestPath("/live?token=abc"))
	assert.Equal(t, "", requestPath("?token=abc"))
	assert.Equal(t, "", requestPath(""))
}
