package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/oapi-codegen/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testJWTSecret = "test-secret-key"

func TestRequestTimeoutSkipsStreamingEndpoints(t *testing.T) {
	for _, path := range []string{"/instances/test/logs", "/builds/test/events"} {
		t.Run(path, func(t *testing.T) {
			handler := timeoutNonStreamingRequests(10 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(30 * time.Millisecond):
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusNoContent, recorder.Code)
		})
	}
}

func TestRequestTimeoutStillAppliesToRegularEndpoints(t *testing.T) {
	handler := timeoutNonStreamingRequests(10 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
}

func generateValidJWT(userID string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	return token.SignedString([]byte(testJWTSecret))
}

func setupTestRouter(t *testing.T) http.Handler {
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	spec.Servers = nil

	r := chi.NewRouter()
	r.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		Options: openapi3filter.Options{
			AuthenticationFunc: mw.OapiAuthenticationFunc(testJWTSecret),
		},
		ErrorHandler: mw.OapiErrorHandler,
	}))

	// Simple handler for testing
	r.Post("/images", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"test"}`))
	})

	return r
}

func TestMiddleware_InvalidPayload(t *testing.T) {
	router := setupTestRouter(t)
	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	// Missing required "name" field
	req := httptest.NewRequest(http.MethodPost, "/images", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMiddleware_InvalidJWT(t *testing.T) {
	router := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/images", bytes.NewBufferString(`{"name":"test"}`))
	req.Header.Set("Authorization", "Bearer invalid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMiddleware_ValidJWT(t *testing.T) {
	router := setupTestRouter(t)
	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/images", bytes.NewBufferString(`{"name":"docker.io/library/nginx:latest"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestOapiRuntimeBindStyledParameter_URLDecoding(t *testing.T) {
	// Test if oapi-codegen's runtime.BindStyledParameterWithOptions URL-decodes path params
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "alpine:latest",
			expected: "alpine:latest",
		},
		{
			name:     "URL-encoded slashes",
			input:    "docker.io%2Flibrary%2Falpine%3Alatest",
			expected: "docker.io/library/alpine:latest", // Should be decoded
		},
		{
			name:     "already decoded",
			input:    "docker.io/library/alpine:latest",
			expected: "docker.io/library/alpine:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dest string
			err := runtime.BindStyledParameterWithOptions(
				"simple", "name", tt.input, &dest,
				runtime.BindStyledParameterOptions{
					ParamLocation: runtime.ParamLocationPath,
					Explode:       false,
					Required:      true,
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, dest, "BindStyledParameterWithOptions should URL-decode the input")
		})
	}
}

func TestGetImage_URLEncodedSlashes(t *testing.T) {
	// This test reproduces the bug where URL-encoded image names are not properly
	// decoded before being passed to the image reference parser.
	//
	// Bug: curl "https://server/images/docker.io%2Flibrary%2Fnginx:alpine"
	// Returns: {"code":"invalid_name","message":"invalid image reference"}
	// Expected: 200 with image details (or 404 if not found)
	//
	// The server passes "docker.io%2Flibrary%2Fnginx:alpine" (still encoded)
	// to the parser instead of "docker.io/library/nginx:alpine" (decoded).

	r := chi.NewRouter()

	// Track what name the handler receives through the oapi wrapper
	var receivedName string

	// Create a handler that implements oapi.ServerInterface
	handler := &testImageHandler{
		getImage: func(w http.ResponseWriter, r *http.Request, name string) {
			receivedName = name
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":"not_found","message":"image not found"}`))
		},
	}

	// Mount using oapi's HandlerFromMux which uses the generated wrappers
	// This tests the full path: chi.URLParam -> runtime.BindStyledParameterWithOptions -> handler
	oapi.HandlerFromMux(handler, r)

	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	tests := []struct {
		name         string
		path         string
		expectedName string
	}{
		{
			name:         "simple image name",
			path:         "/images/alpine:latest",
			expectedName: "alpine:latest",
		},
		{
			name:         "URL-encoded slashes should be decoded",
			path:         "/images/docker.io%2Flibrary%2Fnginx%3Aalpine",
			expectedName: "docker.io/library/nginx:alpine", // Must be decoded!
		},
		{
			name:         "URL-encoded with colon",
			path:         "/images/docker.io%2Flibrary%2Falpine%3Alatest",
			expectedName: "docker.io/library/alpine:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receivedName = "" // Reset

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			// We expect 404 (image not found) - that's fine, we're testing the name decoding
			// NOT 400 (invalid_name) which would indicate the encoded string wasn't decoded
			assert.NotEqual(t, http.StatusBadRequest, w.Code,
				"Got 400 Bad Request - URL-encoded name was likely not decoded. Path: %s, Body: %s",
				tt.path, w.Body.String())

			assert.Equal(t, tt.expectedName, receivedName,
				"Handler received wrong name - URL decoding may have failed")
		})
	}
}

// testImageHandler implements oapi.ServerInterface with just GetImage for testing
type testImageHandler struct {
	oapi.Unimplemented
	getImage func(w http.ResponseWriter, r *http.Request, name string)
}

func (h *testImageHandler) GetImage(w http.ResponseWriter, r *http.Request, name string) {
	if h.getImage != nil {
		h.getImage(w, r, name)
	}
}

func TestTagImageRoute_URLDecodedName(t *testing.T) {
	// Verifies the generated POST /images/{name}/tag route exists and that the
	// URL-encoded source name is decoded before reaching the handler.
	r := chi.NewRouter()

	var receivedName string
	handler := &testTagImageHandler{
		tagImage: func(w http.ResponseWriter, r *http.Request, name string) {
			receivedName = name
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"` + name + `","digest":"sha256:test","status":"ready","created_at":"2026-01-01T00:00:00Z"}`))
		},
	}

	oapi.HandlerFromMux(handler, r)

	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	body := bytes.NewReader([]byte(`{"target":"docker.io/library/alpine:stable"}`))
	req := httptest.NewRequest(http.MethodPost, "/images/docker.io%2Flibrary%2Falpine%3Alatest/tag", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "docker.io/library/alpine:latest", receivedName)
}

// testTagImageHandler implements oapi.ServerInterface with just TagImage for testing
type testTagImageHandler struct {
	oapi.Unimplemented
	tagImage func(w http.ResponseWriter, r *http.Request, name string)
}

func (h *testTagImageHandler) TagImage(w http.ResponseWriter, r *http.Request, name string) {
	if h.tagImage != nil {
		h.tagImage(w, r, name)
	}
}

func TestImageNameWithSlashes_URLEncoding(t *testing.T) {
	// This test verifies how chi router handles image names with slashes.
	// Image names like "docker.io/onkernel/chromium-headful:latest" contain slashes
	// that need to be URL-encoded to work with the /images/{name} endpoint.
	//
	// FINDINGS:
	// 1. Chi DOES route to the handler when slashes are URL-encoded (%2F)
	// 2. Chi's URLParam returns the STILL-ENCODED value (e.g., "docker.io%2Flibrary%2Falpine%3Alatest")
	// 3. The handler/middleware must URL-decode the parameter itself
	// 4. The oapi-codegen runtime.BindStyledParameterWithOptions MAY handle decoding
	//
	// This is a server-side documentation issue - users need to know to URL-encode image names
	// with slashes, and the server needs to ensure proper URL-decoding.

	r := chi.NewRouter()

	var capturedRaw string
	var capturedDecoded string
	r.Get("/images/{name}", func(w http.ResponseWriter, req *http.Request) {
		capturedRaw = chi.URLParam(req, "name")
		// URL-decode the parameter
		decoded, _ := url.QueryUnescape(capturedRaw)
		capturedDecoded = decoded
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"` + capturedDecoded + `"}`))
	})

	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	tests := []struct {
		name            string
		path            string
		expectedStatus  int
		expectedRaw     string
		expectedDecoded string
	}{
		{
			name:            "simple image name (no slashes)",
			path:            "/images/alpine:latest",
			expectedStatus:  http.StatusOK,
			expectedRaw:     "alpine:latest",
			expectedDecoded: "alpine:latest",
		},
		{
			name:            "URL-encoded slashes - routes correctly",
			path:            "/images/docker.io%2Flibrary%2Falpine%3Alatest",
			expectedStatus:  http.StatusOK,
			expectedRaw:     "docker.io%2Flibrary%2Falpine%3Alatest", // chi returns encoded
			expectedDecoded: "docker.io/library/alpine:latest",       // after QueryUnescape
		},
		{
			name:            "unencoded slashes - route not matched",
			path:            "/images/docker.io/library/alpine:latest",
			expectedStatus:  http.StatusNotFound, // chi won't match this route
			expectedRaw:     "",
			expectedDecoded: "",
		},
		{
			name:            "nested image docker.io/onkernel/chromium-headful:latest",
			path:            "/images/docker.io%2Fonkernel%2Fchromium-headful%3Alatest",
			expectedStatus:  http.StatusOK,
			expectedRaw:     "docker.io%2Fonkernel%2Fchromium-headful%3Alatest",
			expectedDecoded: "docker.io/onkernel/chromium-headful:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedRaw = ""
			capturedDecoded = ""

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedStatus, w.Code, "status code mismatch for path %s", tt.path)
			if tt.expectedStatus == http.StatusOK {
				assert.Equal(t, tt.expectedRaw, capturedRaw, "raw captured name mismatch")
				assert.Equal(t, tt.expectedDecoded, capturedDecoded, "decoded name mismatch")
			}
		})
	}
}

type vgpuReconcileManagerStub struct {
	instances.Manager
	list []instances.Instance
}

func (s vgpuReconcileManagerStub) ListInstancesForReconcile(context.Context) ([]instances.Instance, error) {
	return s.list, nil
}

func TestLiveInstanceVGPUDevicePathsRequiresReconcileInventory(t *testing.T) {
	_, _, err := liveInstanceVGPUDevicePaths(context.Background(), struct{ instances.Manager }{})
	require.ErrorContains(t, err, "does not support vGPU reconcile inventory")
}

func TestLiveInstanceVGPUDevicePathsBoundsStartupProtection(t *testing.T) {
	dead := exec.Command("true")
	require.NoError(t, dead.Run())
	deadPID := dead.Process.Pid
	recent := time.Now().Add(-time.Minute)
	stale := time.Now().Add(-instances.VGPUAssignmentStartupGracePeriod - time.Minute)

	manager := vgpuReconcileManagerStub{list: []instances.Instance{
		{StoredMetadata: instances.StoredMetadata{Id: "booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4", GPUAssignedAt: &recent}},
		{StoredMetadata: instances.StoredMetadata{Id: "orphaned", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.5", GPUAssignedAt: &stale}},
		{StoredMetadata: instances.StoredMetadata{Id: "legacy", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.6"}},
		{StoredMetadata: instances.StoredMetadata{Id: "dead", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.7", HypervisorProcessIdentity: instances.HypervisorProcessIdentity{HypervisorPID: &deadPID}}},
		{StoredMetadata: instances.StoredMetadata{Id: "stale-pid-booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.8", HypervisorProcessIdentity: instances.HypervisorProcessIdentity{HypervisorPID: &deadPID}, GPUAssignedAt: &recent}},
	}}

	protected, retryAfter, err := liveInstanceVGPUDevicePaths(context.Background(), manager)
	require.NoError(t, err)
	require.Positive(t, retryAfter)
	require.LessOrEqual(t, retryAfter, instances.VGPUAssignmentStartupGracePeriod)
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.4")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.5")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.6")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.7")
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.8")
}
