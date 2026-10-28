package api

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

func machineMultipart(t *testing.T, fields ...[2]string) *multipart.Reader {
	t.Helper()
	var data bytes.Buffer
	writer := multipart.NewWriter(&data)
	for _, field := range fields {
		require.NoError(t, writer.WriteField(field[0], field[1]))
	}
	require.NoError(t, writer.Close())
	return multipart.NewReader(&data, writer.Boundary())
}

type machineZeroReader struct{}

func (machineZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestMachineBuildBodyBoundBeforeValidation(t *testing.T) {
	for _, path := range []string{"/builds", "/images"} {
		r := authorizedMachineBuildRequest(t, path, io.LimitReader(machineZeroReader{}, (65<<20)+1))
		LimitMachineBuildBody(machineBuildTestSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, err := io.Copy(io.Discard, r.Body)
			if path == "/builds" {
				require.Error(t, err)
				require.EqualValues(t, 65<<20, n)
			} else {
				require.NoError(t, err)
				require.EqualValues(t, (65<<20)+1, n)
			}
		})).ServeHTTP(httptest.NewRecorder(), r)
	}
}

func TestMachineBuildUploadsAreBounded(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, maxConcurrentMachineBuildUploads)
	handler := LimitMachineBuildBody(machineBuildTestSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	done := make(chan struct{})
	for range maxConcurrentMachineBuildUploads {
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), authorizedMachineBuildRequest(t, "/builds", nil))
			done <- struct{}{}
		}()
		<-entered
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedMachineBuildRequest(t, "/builds", nil))
	require.Equal(t, http.StatusTooManyRequests, w.Code)

	close(release)
	for range maxConcurrentMachineBuildUploads {
		<-done
	}
	w = httptest.NewRecorder()
	go func() { <-entered }()
	handler.ServeHTTP(w, authorizedMachineBuildRequest(t, "/builds", nil))
	require.Equal(t, http.StatusOK, w.Code, "a slot frees once an upload finishes")
}

func TestMachineBuildMultipartAdmission(t *testing.T) {
	base := "example.test/base@sha256:" + strings.Repeat("a", 64)
	valid := [][2]string{{"source", "synthetic"}, {"machine_base_image", base}}
	for _, tc := range []struct {
		name  string
		extra [][2]string
		ok    bool
	}{
		{name: "inherit", ok: true},
		{name: "policy", extra: [][2]string{{"cpus", "4"}, {"memory_mb", "8192"}, {"timeout_seconds", "1200"}}, ok: true},
		{name: "isolated network", extra: [][2]string{{"machine_network_mode", "isolated"}}, ok: true},
		{name: "egress network", extra: [][2]string{{"machine_network_mode", "egress"}}, ok: true},
		{name: "invalid network", extra: [][2]string{{"machine_network_mode", "bridge"}}},
		{name: "empty network", extra: [][2]string{{"machine_network_mode", ""}}},
		{name: "duplicate", extra: [][2]string{{"source", "second"}}},
		{name: "linux", extra: [][2]string{{"dockerfile", "FROM scratch"}}},
		{name: "secret", extra: [][2]string{{"secrets", "[]"}}},
		{name: "invalid number", extra: [][2]string{{"cpus", "nan"}}},
		{name: "negative", extra: [][2]string{{"memory_mb", "-1"}}},
		{name: "timeout", extra: [][2]string{{"timeout_seconds", "86401"}}},
		{name: "metadata bound", extra: [][2]string{{"tags", strings.Repeat("x", (64<<10)+1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, source, err := parseMachineBuildMultipart(machineMultipart(t, append(append([][2]string{}, valid...), tc.extra...)...))
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, base, req.MachineBaseImage)
			require.Equal(t, []byte("synthetic"), source)
			if tc.name == "egress network" {
				require.Equal(t, "egress", req.BuildPolicy.NetworkMode)
			}
		})
	}
	_, _, err := parseMachineBuildMultipart(machineMultipart(t, [2]string{"source", "synthetic"}, [2]string{"machine_base_image", "example.test/base:latest"}))
	require.Error(t, err)
}

type machineHTTPManager struct {
	builds.Manager
	req   builds.CreateBuildRequest
	calls int
}

func (m *machineHTTPManager) CreateBuild(_ context.Context, req builds.CreateBuildRequest, source []byte) (*builds.Build, error) {
	m.calls++
	m.req = req
	return &builds.Build{ID: "synthetic-build", Status: builds.StatusQueued}, nil
}

func TestMachineBuildHTTPGateAndDispatch(t *testing.T) {
	base := "example.test/base@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name     string
		cfg      *config.Config
		accepted bool
	}{
		{"disabled", &config.Config{MacOSOnly: true}, false},
		{"linux", &config.Config{}, false},
		{"enabled", &config.Config{MacOSOnly: true, MacOSBuilds: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &machineHTTPManager{}
			service := &ApiService{Config: tc.cfg, BuildManager: manager}
			response, err := service.CreateBuild(context.Background(), oapi.CreateBuildRequestObject{Body: machineMultipart(t, [2]string{"source", "synthetic"}, [2]string{"machine_base_image", base})})
			require.NoError(t, err)
			if tc.accepted {
				require.IsType(t, oapi.CreateBuild202JSONResponse{}, response)
				require.Equal(t, 1, manager.calls)
				require.Equal(t, base, manager.req.MachineBaseImage)
			} else {
				// Disabled machine builds are unsupported (501) on macOS-only servers, and
				// a Linux server rejects the machine selector as a bad request (400).
				if tc.cfg.MacOSOnly {
					require.IsType(t, oapi.CreateBuild501JSONResponse{}, response)
				} else {
					require.IsType(t, oapi.CreateBuild400JSONResponse{}, response)
				}
				require.Zero(t, manager.calls)
			}
		})
	}
}

const machineBuildTestSecret = "machine-build-test-secret"

func authorizedMachineBuildRequest(t *testing.T, path string, body io.Reader) *http.Request {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(machineBuildTestSecret))
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, path, body)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestMachineBuildUploadsRequireWriteScopeBeforeTakingASlot(t *testing.T) {
	claims := jwt.MapClaims{
		"sub":         "user",
		"iat":         time.Now().Unix(),
		"exp":         time.Now().Add(time.Hour).Unix(),
		"permissions": []string{"build:read"},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(machineBuildTestSecret))
	require.NoError(t, err)
	called := false
	handler := LimitMachineBuildBody(machineBuildTestSecret)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest(http.MethodPost, "/builds", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, called, "read-only token must be rejected before upload processing")
}

func TestMachineBuildUploadsRequireAuthBeforeTakingASlot(t *testing.T) {
	reached := false
	handler := LimitMachineBuildBody(machineBuildTestSecret)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	for range maxConcurrentMachineBuildUploads + 1 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/builds", nil))
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}
	require.False(t, reached)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedMachineBuildRequest(t, "/builds", nil))
	require.Equal(t, http.StatusOK, w.Code, "unauthenticated requests hold no upload slot")
	require.True(t, reached)
}
