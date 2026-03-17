package scopes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopeValid(t *testing.T) {
	assert.True(t, InstanceRead.Valid())
	assert.True(t, All.Valid())
	assert.False(t, Scope("bogus:scope").Valid())
	assert.False(t, Scope("").Valid())
}

func TestParseScopes(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		s, err := ParseScopes("")
		require.NoError(t, err)
		assert.Nil(t, s)
	})

	t.Run("single scope", func(t *testing.T) {
		s, err := ParseScopes("instance:read")
		require.NoError(t, err)
		assert.Equal(t, []Scope{InstanceRead}, s)
	})

	t.Run("multiple scopes", func(t *testing.T) {
		s, err := ParseScopes("instance:read,instance:write,image:read")
		require.NoError(t, err)
		assert.Equal(t, []Scope{InstanceRead, InstanceWrite, ImageRead}, s)
	})

	t.Run("wildcard", func(t *testing.T) {
		s, err := ParseScopes("*")
		require.NoError(t, err)
		assert.Equal(t, []Scope{All}, s)
	})

	t.Run("trims whitespace", func(t *testing.T) {
		s, err := ParseScopes("instance:read , image:read")
		require.NoError(t, err)
		assert.Equal(t, []Scope{InstanceRead, ImageRead}, s)
	})

	t.Run("unknown scope returns error", func(t *testing.T) {
		_, err := ParseScopes("instance:read,bogus:thing")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unknown scope")
	})
}

func TestHasScope(t *testing.T) {
	t.Run("nil permissions means full access", func(t *testing.T) {
		ctx := context.Background()
		assert.True(t, HasScope(ctx, InstanceRead))
		assert.True(t, HasScope(ctx, InstanceDelete))
	})

	t.Run("wildcard grants everything", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{All})
		assert.True(t, HasScope(ctx, InstanceRead))
		assert.True(t, HasScope(ctx, BuildDelete))
	})

	t.Run("specific scopes", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{InstanceRead, ImageRead})
		assert.True(t, HasScope(ctx, InstanceRead))
		assert.True(t, HasScope(ctx, ImageRead))
		assert.False(t, HasScope(ctx, InstanceWrite))
		assert.False(t, HasScope(ctx, BuildRead))
	})

	t.Run("empty permissions slice denies all", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{})
		assert.False(t, HasScope(ctx, InstanceRead))
	})
}

func TestHasFullAccess(t *testing.T) {
	t.Run("no permissions set", func(t *testing.T) {
		assert.True(t, HasFullAccess(context.Background()))
	})
	t.Run("wildcard set", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{All})
		assert.True(t, HasFullAccess(ctx))
	})
	t.Run("limited scopes", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{InstanceRead})
		assert.False(t, HasFullAccess(ctx))
	})
}

func TestRequireScope(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("allows when scope present", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{InstanceRead})
		req := httptest.NewRequest(http.MethodGet, "/instances", nil).WithContext(ctx)
		rr := httptest.NewRecorder()

		RequireScope(InstanceRead)(handler).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("allows legacy tokens without permissions", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		rr := httptest.NewRecorder()

		RequireScope(InstanceRead)(handler).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("rejects when scope missing", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{ImageRead})
		req := httptest.NewRequest(http.MethodGet, "/instances", nil).WithContext(ctx)
		rr := httptest.NewRecorder()

		RequireScope(InstanceRead)(handler).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.Contains(t, rr.Body.String(), "instance:read")
	})

	t.Run("wildcard grants access", func(t *testing.T) {
		ctx := ContextWithPermissions(context.Background(), []Scope{All})
		req := httptest.NewRequest(http.MethodGet, "/instances", nil).WithContext(ctx)
		rr := httptest.NewRecorder()

		RequireScope(InstanceRead)(handler).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

func TestScopeForRoute(t *testing.T) {
	s, ok := ScopeForRoute("GET", "/instances")
	assert.True(t, ok)
	assert.Equal(t, InstanceRead, s)

	s, ok = ScopeForRoute("POST", "/instances")
	assert.True(t, ok)
	assert.Equal(t, InstanceWrite, s)

	s, ok = ScopeForRoute("DELETE", "/instances/{id}")
	assert.True(t, ok)
	assert.Equal(t, InstanceDelete, s)

	_, ok = ScopeForRoute("GET", "/nonexistent")
	assert.False(t, ok)
}

func TestScopeStrings(t *testing.T) {
	s := ScopeStrings([]Scope{InstanceRead, ImageWrite})
	assert.Equal(t, []string{"instance:read", "image:write"}, s)
}
