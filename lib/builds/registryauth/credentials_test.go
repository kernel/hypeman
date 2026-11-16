package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeAndRedact(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	password := `synthetic-"secret"/+=<&>`
	input := map[string]Credential{"REGISTRY.EXAMPLE": {Username: "AWS", Password: password, ExpiresAt: &expires}}
	got, err := Normalize(input, "http://local.example:4973", time.Now())
	require.NoError(t, err)
	require.Equal(t, password, got["registry.example"].Password)
	input["REGISTRY.EXAMPLE"] = Credential{}
	expires = time.Time{}
	require.True(t, got["registry.example"].ExpiresAt.After(time.Now()))
	redact := Redactor(got)
	quoted, err := json.Marshal(password)
	require.NoError(t, err)
	for _, value := range []string{password, string(quoted), strconv.Quote(password), base64.StdEncoding.EncodeToString([]byte("AWS:" + password))} {
		require.NotContains(t, redact("before "+value+" after"), value)
		require.Contains(t, redact(value), "[REDACTED]")
	}
	for _, host := range []string{"https://registry.example", "registry.example/path", "registry.example?x=y", "user:pass@registry.example", "local.example:4973", "registry.example\n"} {
		_, err := Normalize(map[string]Credential{host: {Username: "AWS", Password: password}}, "local.example:4973", time.Now())
		require.ErrorIs(t, err, ErrInvalid)
		require.NotContains(t, err.Error(), password)
	}
	for _, credential := range []Credential{{Username: "AWS"}, {Password: password}, {Username: "AWS", Password: "line\nbreak"}, {Username: "AWS", Password: password, ExpiresAt: &expires}, {Username: "AWS", Password: strings.Repeat("x", MaxBytes+1)}} {
		_, err := Normalize(map[string]Credential{"registry.example": credential}, "local.example:4973", time.Now())
		require.ErrorIs(t, err, ErrInvalid)
	}
	_, err = Normalize(map[string]Credential{"docker.io": {Username: "u", Password: "p"}, "index.docker.io": {Username: "u", Password: "p"}}, "local.example:4973", time.Now())
	require.ErrorIs(t, err, ErrInvalid)
}
