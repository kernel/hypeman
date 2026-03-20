package oapi

import (
	"bytes"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeOptionalJSONBody(t *testing.T) {
	t.Parallel()

	t.Run("no body returns nil", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("POST", "/instances/test/standby", nil)
		var body StandbyInstanceJSONRequestBody

		decoded, err := decodeOptionalJSONBody(req, &body)
		require.NoError(t, err)
		assert.Nil(t, decoded)
	})

	t.Run("empty reader returns nil", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("POST", "/instances/test/standby", io.NopCloser(bytes.NewBuffer(nil)))
		var body StandbyInstanceJSONRequestBody

		decoded, err := decodeOptionalJSONBody(req, &body)
		require.NoError(t, err)
		assert.Nil(t, decoded)
	})

	t.Run("json object decodes", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("POST", "/instances/test/standby", bytes.NewBufferString(`{"compression":{"enabled":true,"algorithm":"lz4"}}`))
		var body StandbyInstanceJSONRequestBody

		decoded, err := decodeOptionalJSONBody(req, &body)
		require.NoError(t, err)
		require.NotNil(t, decoded)
		require.NotNil(t, decoded.Compression)
		assert.True(t, decoded.Compression.Enabled)
		require.NotNil(t, decoded.Compression.Algorithm)
		assert.Equal(t, SnapshotCompressionConfigAlgorithm("lz4"), *decoded.Compression.Algorithm)
	})
}
