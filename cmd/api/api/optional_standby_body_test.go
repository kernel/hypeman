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

func TestNormalizeOptionalStandbyBody(t *testing.T) {
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

		NormalizeOptionalStandbyBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, []byte(`{}`), gotBody)
		assert.Equal(t, "application/json", gotContentType)
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

		NormalizeOptionalStandbyBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, []byte(`{"compression":{"enabled":true}}`), gotBody)
	})

	t.Run("non-standby route is untouched", func(t *testing.T) {
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

		NormalizeOptionalStandbyBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, gotBody)
	})

	t.Run("non-post request skips standby normalization", func(t *testing.T) {
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

		NormalizeOptionalStandbyBody(next).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, gotBody)
	})

	t.Run("standby route matcher only accepts single path segment ids", func(t *testing.T) {
		t.Parallel()

		assert.True(t, isStandbyRoutePath("/instances/test/standby"))
		assert.False(t, isStandbyRoutePath("/instances/test/start"))
		assert.False(t, isStandbyRoutePath("/instances/test/standby/extra"))
		assert.False(t, isStandbyRoutePath("/instances/test/nested/standby"))
	})
}
