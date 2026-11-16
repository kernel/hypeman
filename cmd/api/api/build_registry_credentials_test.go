package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/builds/registryauth"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

type registryCredentialBuildManager struct {
	builds.Manager
	request builds.CreateBuildRequest
}

func (m *registryCredentialBuildManager) CreateBuild(_ context.Context, req builds.CreateBuildRequest, _ []byte) (*builds.Build, error) {
	m.request = req
	if _, err := registryauth.Normalize(req.RegistryCredentials, "local.example:4973", time.Now()); err != nil {
		return nil, err
	}
	return &builds.Build{ID: "test-build", Status: builds.StatusQueued}, nil
}

func TestCreateBuildRegistryCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"valid", `{"registry.example":{"username":"AWS","password":"synthetic-token"}}`, true},
		{"omitted", "", true},
		{"malformed", `{"password":"synthetic-token"`, false},
		{"bad-host", `{"https://registry.example":{"username":"AWS","password":"synthetic-token"}}`, false},
		{"local-registry", `{"local.example:4973":{"username":"AWS","password":"synthetic-token"}}`, false},
		{"expired", `{"registry.example":{"username":"AWS","password":"synthetic-token","expires_at":"2000-01-01T00:00:00Z"}}`, false},
		{"oversized", strings.Repeat("x", registryauth.MaxBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &registryCredentialBuildManager{}
			svc := &ApiService{BuildManager: manager}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			require.NoError(t, writer.WriteField("source", "source"))
			if tc.data != "" {
				require.NoError(t, writer.WriteField("registry_credentials", tc.data))
			}
			require.NoError(t, writer.Close())
			response, err := svc.CreateBuild(context.Background(), oapi.CreateBuildRequestObject{Body: multipart.NewReader(&body, writer.Boundary())})
			require.NoError(t, err)
			if tc.valid {
				_, ok := response.(oapi.CreateBuild202JSONResponse)
				require.True(t, ok)
				if tc.data != "" {
					require.Equal(t, "synthetic-token", manager.request.RegistryCredentials["registry.example"].Password)
				}
			} else {
				_, ok := response.(oapi.CreateBuild400JSONResponse)
				require.True(t, ok)
			}
			encoded, err := json.Marshal(response)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "synthetic-token")
		})
	}
}
