package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"testing"

	"github.com/stretchr/testify/require"
)

// A proxied server may send 103 Early Hints before its real status. The wrapper must
// pass the informational response through and still send and record the final one.
func TestResponseWriterPassesInformationalResponses(t *testing.T) {
	recorded := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := wrapResponseWriter(w)
		rw.Header().Set("Link", "</app.css>; rel=preload")
		rw.WriteHeader(http.StatusEarlyHints)
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte("missing"))
		recorded <- rw.Status()
	}))
	defer srv.Close()

	var informational []int
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			informational = append(informational, code)
			return nil
		},
	}))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, []int{http.StatusEarlyHints}, informational)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, http.StatusNotFound, <-recorded)
}
