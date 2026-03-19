// Package scopes defines API permission scopes for hypeman API keys.
//
// Scopes follow the pattern "resource:action" where resource is one of the
// API resource types and action is read, write, or delete. Tokens without
// a "permissions" claim are treated as having full access for backward
// compatibility with existing tokens.
package scopes

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Scope represents a permission scope for API access.
type Scope string

const (
	// Instance scopes
	InstanceRead   Scope = "instance:read"
	InstanceWrite  Scope = "instance:write" // create, start, stop, standby, restore, fork, exec, cp
	InstanceDelete Scope = "instance:delete"

	// Image scopes
	ImageRead   Scope = "image:read"
	ImageWrite  Scope = "image:write" // pull/create
	ImageDelete Scope = "image:delete"

	// Volume scopes
	VolumeRead   Scope = "volume:read"
	VolumeWrite  Scope = "volume:write" // create, attach, detach
	VolumeDelete Scope = "volume:delete"

	// Snapshot scopes
	SnapshotRead   Scope = "snapshot:read"
	SnapshotWrite  Scope = "snapshot:write" // create, restore, fork
	SnapshotDelete Scope = "snapshot:delete"

	// Build scopes
	BuildRead   Scope = "build:read"
	BuildWrite  Scope = "build:write"  // create
	BuildDelete Scope = "build:delete" // cancel

	// Device scopes
	DeviceRead   Scope = "device:read"
	DeviceWrite  Scope = "device:write"  // register
	DeviceDelete Scope = "device:delete" // unregister

	// Ingress scopes
	IngressRead   Scope = "ingress:read"
	IngressWrite  Scope = "ingress:write"
	IngressDelete Scope = "ingress:delete"

	// Resource/health scopes (read-only)
	ResourceRead Scope = "resource:read"

	// Wildcard scope — grants all permissions
	All Scope = "*"
)

// allScopes is the complete list of valid scopes (excluding wildcard).
var allScopes = []Scope{
	InstanceRead, InstanceWrite, InstanceDelete,
	ImageRead, ImageWrite, ImageDelete,
	VolumeRead, VolumeWrite, VolumeDelete,
	SnapshotRead, SnapshotWrite, SnapshotDelete,
	BuildRead, BuildWrite, BuildDelete,
	DeviceRead, DeviceWrite, DeviceDelete,
	IngressRead, IngressWrite, IngressDelete,
	ResourceRead,
}

// AllScopes returns the complete list of valid scopes (excluding wildcard).
func AllScopes() []Scope {
	out := make([]Scope, len(allScopes))
	copy(out, allScopes)
	return out
}

// Valid returns true if s is a recognized scope.
func (s Scope) Valid() bool {
	if s == All {
		return true
	}
	return slices.Contains(allScopes, s)
}

// ParseScopes parses a comma-separated scope string into a slice of Scopes.
// Returns an error if any scope is unrecognized.
func ParseScopes(s string) ([]Scope, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]Scope, 0, len(parts))
	for _, p := range parts {
		sc := Scope(strings.TrimSpace(p))
		if !sc.Valid() {
			return nil, fmt.Errorf("unknown scope: %q", sc)
		}
		out = append(out, sc)
	}
	return out, nil
}

// ScopeStrings converts a slice of Scopes to strings.
func ScopeStrings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

type permissionsContextKey struct{}

// HasFullAccess returns true if the request context indicates full access
// (no scopes restriction). This is the case for legacy tokens without
// a permissions claim.
func HasFullAccess(ctx context.Context) bool {
	v, ok := ctx.Value(permissionsContextKey{}).([]Scope)
	if !ok {
		// No permissions set — means full access (legacy token)
		return true
	}
	return slices.Contains(v, All)
}

// ContextWithPermissions stores the granted scopes in the context.
// A nil slice means full access (legacy behavior).
func ContextWithPermissions(ctx context.Context, perms []Scope) context.Context {
	return context.WithValue(ctx, permissionsContextKey{}, perms)
}

// GetPermissions extracts the granted scopes from context.
// Returns nil if no permissions are set (legacy full-access token).
func GetPermissions(ctx context.Context) []Scope {
	v, _ := ctx.Value(permissionsContextKey{}).([]Scope)
	return v
}

// HasScope checks whether the context has the required scope.
// Returns true if:
//   - No permissions claim was set (legacy full-access token)
//   - The wildcard scope "*" is present
//   - The specific scope is present
func HasScope(ctx context.Context, required Scope) bool {
	perms := GetPermissions(ctx)
	if perms == nil {
		return true // legacy token — full access
	}
	if slices.Contains(perms, All) {
		return true
	}
	return slices.Contains(perms, required)
}

// RequireScope returns an HTTP middleware that rejects requests lacking
// the specified scope with 403 Forbidden.
func RequireScope(required Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !HasScope(r.Context(), required) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, `{"code":"Forbidden","message":"missing required scope: %s"}`, required)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// PublicRoutes lists route keys ("METHOD /path") that are intentionally
// unscoped — they do not require authentication or scope checks.
// The test uses this to distinguish "intentionally public" from "forgot to
// add a scope mapping".
var PublicRoutes = map[string]bool{
	"GET /spec.yaml": true,
	"GET /spec.json": true,
	"GET /swagger":   true,
}

// DirectScopeRoutes lists route keys that enforce scopes via RequireScope
// middleware directly (not via the Middleware() scope checker). These are
// outside the OpenAPI router group (e.g. WebSocket endpoints).
var DirectScopeRoutes = map[string]Scope{
	"GET /instances/{id}/exec": InstanceWrite,
	"GET /instances/{id}/cp":   InstanceWrite,
}

// RouteScopes maps "METHOD /path-pattern" to the required scope.
// Path patterns use chi-style {param} placeholders.
var RouteScopes = map[string]Scope{
	// Builds
	"GET /builds":             BuildRead,
	"POST /builds":            BuildWrite,
	"DELETE /builds/{id}":     BuildDelete,
	"GET /builds/{id}":        BuildRead,
	"GET /builds/{id}/events": BuildRead,

	// Devices
	"GET /devices":           DeviceRead,
	"POST /devices":          DeviceWrite,
	"GET /devices/available": DeviceRead,
	"DELETE /devices/{id}":   DeviceDelete,
	"GET /devices/{id}":      DeviceRead,

	// Health & Resources
	"GET /health":    ResourceRead,
	"GET /resources": ResourceRead,

	// Images
	"GET /images":           ImageRead,
	"POST /images":          ImageWrite,
	"DELETE /images/{name}": ImageDelete,
	"GET /images/{name}":    ImageRead,

	// Ingresses
	"GET /ingresses":         IngressRead,
	"POST /ingresses":        IngressWrite,
	"DELETE /ingresses/{id}": IngressDelete,
	"GET /ingresses/{id}":    IngressRead,

	// Instances
	"GET /instances":                                      InstanceRead,
	"POST /instances":                                     InstanceWrite,
	"DELETE /instances/{id}":                              InstanceDelete,
	"GET /instances/{id}":                                 InstanceRead,
	"POST /instances/{id}/fork":                           InstanceWrite,
	"GET /instances/{id}/logs":                            InstanceRead,
	"POST /instances/{id}/restore":                        InstanceWrite,
	"POST /instances/{id}/snapshots":                      SnapshotWrite,
	"POST /instances/{id}/snapshots/{snapshotId}/restore": SnapshotWrite,
	"POST /instances/{id}/standby":                        InstanceWrite,
	"POST /instances/{id}/start":                          InstanceWrite,
	"GET /instances/{id}/stat":                            InstanceRead,
	"GET /instances/{id}/stats":                           InstanceRead,
	"POST /instances/{id}/stop":                           InstanceWrite,
	"PATCH /instances/{id}/env":                            InstanceWrite,
	"DELETE /instances/{id}/volumes/{volumeId}":           VolumeWrite,
	"POST /instances/{id}/volumes/{volumeId}":             VolumeWrite,

	// Snapshots
	"GET /snapshots":                    SnapshotRead,
	"DELETE /snapshots/{snapshotId}":    SnapshotDelete,
	"GET /snapshots/{snapshotId}":       SnapshotRead,
	"POST /snapshots/{snapshotId}/fork": SnapshotWrite,

	// Volumes
	"GET /volumes":               VolumeRead,
	"POST /volumes":              VolumeWrite,
	"POST /volumes/from-archive": VolumeWrite,
	"DELETE /volumes/{id}":       VolumeDelete,
	"GET /volumes/{id}":          VolumeRead,
}

// ScopeForRoute looks up the required scope for a given HTTP method and
// chi route pattern (e.g. "GET", "/instances/{id}"). Returns the scope
// and true if found, or ("", false) if the route is not mapped.
func ScopeForRoute(method, pattern string) (Scope, bool) {
	key := method + " " + pattern
	s, ok := RouteScopes[key]
	return s, ok
}
