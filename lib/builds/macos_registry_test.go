package builds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMachineFetchVerifiesSizeAndDigest(t *testing.T) {
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
		size   int64
		ok     bool
	}{
		{"metadata matches digest", server.URL, digest, 0, true},
		{"sized object matches", server.URL, digest, int64(len(body)), true},
		{"wrong digest", server.URL, machineHash([]byte("other")).String(), 0, false},
		{"size shorter than body", server.URL, digest, int64(len(body)) - 1, false},
		{"size longer than body", server.URL, digest, int64(len(body)) + 1, false},
		{"metadata over cap", bigServer.URL, machineHash([]byte(oversize)).String(), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := machineFetch(context.Background(), http.DefaultClient, "token", tc.target, tc.digest, tc.size)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.size == 0 {
				require.Equal(t, body, got, "metadata is returned")
			} else {
				require.Nil(t, got, "sized objects are verified, never buffered")
			}
		})
	}
}
