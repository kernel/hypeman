package builds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMachineFetchVerifiesDigest(t *testing.T) {
	body := []byte("synthetic object")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer server.Close()
	digest := machineHash(body).String()
	oversize := strings.Repeat("x", maxMachineMetadataBytes+1)
	bigServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(oversize))
	}))
	defer bigServer.Close()

	for _, tc := range []struct {
		name   string
		target string
		digest string
		ok     bool
	}{
		{"matches digest", server.URL, digest, true},
		{"wrong digest", server.URL, machineHash([]byte("other")).String(), false},
		{"over cap", bigServer.URL, machineHash([]byte(oversize)).String(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := machineFetch(context.Background(), http.DefaultClient, "token", tc.target, tc.digest)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, body, got)
		})
	}
}
