package ingress

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReloadConfigErrorDoesNotExposeCaddyResponse(t *testing.T) {
	const secret = "DO_NOT_EXPOSE"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, fmt.Sprintf("invalid matcher value %s", secret), http.StatusBadRequest)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(endpoint.Port())
	require.NoError(t, err)
	daemon := &CaddyDaemon{adminAddress: endpoint.Hostname(), adminPort: port}

	err = daemon.ReloadConfig([]byte(`{"apps":{}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
	assert.NotContains(t, err.Error(), secret)
}
