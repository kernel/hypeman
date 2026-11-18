package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOptionalBody(t *testing.T) {
	t.Parallel()

	t.Run("empty standby body becomes empty JSON object", func(t *testing.T) {
		t.Parallel()

		var gotBody []byte
		var gotContentType string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			gotBody, err = io.ReadAll(r.Body)
			require.NoError(t, err)
			gotContentType = r.Header.Get("Content-Type")
			w.WriteHeader(http.StatusNoContent)
		})

		req := httptest.NewRequest(http.MethodPost, "/instances/test/standby", nil)
		rec := httptest.NewRecorder()

		NormalizeOptionalBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, []byte(`{}`), gotBody)
		assert.Equal(t, "application/json", gotContentType)
	})

	t.Run("empty stop body becomes empty JSON object", func(t *testing.T) {
		t.Parallel()

		var gotBody []byte
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			gotBody, err = io.ReadAll(r.Body)
			require.NoError(t, err)
			w.WriteHeader(http.StatusNoContent)
		})

		req := httptest.NewRequest(http.MethodPost, "/instances/test/stop", nil)
		rec := httptest.NewRecorder()

		NormalizeOptionalBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, []byte(`{}`), gotBody)
	})

	t.Run("existing standby body is preserved", func(t *testing.T) {
		t.Parallel()

		var gotBody []byte
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			gotBody, err = io.ReadAll(r.Body)
			require.NoError(t, err)
			w.WriteHeader(http.StatusNoContent)
		})

		req := httptest.NewRequest(http.MethodPost, "/instances/test/standby", bytes.NewBufferString(`{"compression":{"enabled":true}}`))
		rec := httptest.NewRecorder()

		NormalizeOptionalBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, []byte(`{"compression":{"enabled":true}}`), gotBody)
	})

	t.Run("route without an optional body is untouched", func(t *testing.T) {
		t.Parallel()

		var gotBody []byte
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			gotBody, err = io.ReadAll(r.Body)
			require.NoError(t, err)
			w.WriteHeader(http.StatusNoContent)
		})

		req := httptest.NewRequest(http.MethodPost, "/instances/test/start", nil)
		rec := httptest.NewRecorder()

		NormalizeOptionalBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, gotBody)
	})

	t.Run("non-post request skips normalization", func(t *testing.T) {
		t.Parallel()

		var gotBody []byte
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			gotBody, err = io.ReadAll(r.Body)
			require.NoError(t, err)
			w.WriteHeader(http.StatusNoContent)
		})

		req := httptest.NewRequest(http.MethodGet, "/instances/test/standby", nil)
		rec := httptest.NewRecorder()

		NormalizeOptionalBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, gotBody)
	})

	t.Run("route matcher only accepts single path segment ids", func(t *testing.T) {
		t.Parallel()

		assert.True(t, isOptionalBodyRoutePath("/instances/test/standby"))
		assert.True(t, isOptionalBodyRoutePath("/instances/test/stop"))
		assert.False(t, isOptionalBodyRoutePath("/instances/test/start"))
		assert.False(t, isOptionalBodyRoutePath("/instances/test/standby/extra"))
		assert.False(t, isOptionalBodyRoutePath("/instances/test/nested/standby"))
		assert.False(t, isOptionalBodyRoutePath("/instances/test/nested/stop"))
	})
}
