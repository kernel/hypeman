// Package registry implements an OCI Distribution Spec registry.
package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenHandler handles Docker registry token authentication requests.
// This implements the Docker Registry v2 authentication specification.
//
// Flow:
// 1. Client requests /v2/... without auth
// 2. Server returns 401 with WWW-Authenticate: Bearer realm="https://host/v2/token",service="registry"
// 3. Client requests /v2/token with Basic auth (using our JWT as username)
// 4. Server validates the JWT and returns a short-lived access token
// 5. Client retries /v2/... with Authorization: Bearer <access_token>
type TokenHandler struct {
	jwtSecret string
	service   string
}

// NewTokenHandler creates a new token endpoint handler.
func NewTokenHandler(jwtSecret, service string) *TokenHandler {
	return &TokenHandler{
		jwtSecret: jwtSecret,
		service:   service,
	}
}

// RegistryTokenClaims contains the claims from a registry access token.
// This mirrors the type in middleware to avoid circular imports.
type RegistryTokenClaims struct {
	jwt.RegisteredClaims
	BuildID string `json:"build_id"`

	// RepoAccess defines per-repository access permissions (new two-tier format)
	RepoAccess []RepoPermission `json:"repo_access,omitempty"`

	// Repositories is the list of allowed repository paths (legacy format)
	Repositories []string `json:"repos,omitempty"`

	// Scope is the access scope (legacy format)
	Scope string `json:"scope,omitempty"`
}

// RepoPermission defines access permissions for a specific repository.
type RepoPermission struct {
	Repo  string `json:"repo"`
	Scope string `json:"scope"`
}

// TokenResponse is the response format for the token endpoint.
// This follows the Docker Registry Token Authentication spec.
type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token,omitempty"` // Alias for compatibility
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at,omitempty"`
}

// AccessTokenClaims are the claims in the access token we issue.
// This follows the Docker registry token format.
type AccessTokenClaims struct {
	jwt.RegisteredClaims
	Access []AccessEntry `json:"access,omitempty"`
}

// AccessEntry describes access to a specific resource.
type AccessEntry struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// ServeHTTP handles token requests.
// Query parameters:
//   - service: the service requesting the token (must match our service)
//   - scope: requested scope in format "repository:name:actions"
//
// The client should provide Basic auth with the original JWT as username.
// Anonymous requests (no auth) return a token with no permissions.
func (h *TokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract Basic auth credentials
	authHeader := r.Header.Get("Authorization")
	
	// If no auth provided, return an empty token (no permissions)
	// This follows the Docker registry spec for anonymous access
	if authHeader == "" {
		h.returnAnonymousToken(w, r)
		return
	}

	if !strings.HasPrefix(authHeader, "Basic ") {
		// Try Bearer auth (client might send our original JWT as Bearer)
		if strings.HasPrefix(authHeader, "Bearer ") {
			// Treat Bearer token as the JWT credential
			authHeader = "Basic " + base64.StdEncoding.EncodeToString(
				[]byte(strings.TrimPrefix(authHeader, "Bearer ")+":"))
		} else {
			h.errorResponse(w, "basic or bearer auth required", http.StatusUnauthorized)
			return
		}
	}

	// Decode Basic auth
	encoded := strings.TrimPrefix(authHeader, "Basic ")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		h.errorResponse(w, "invalid basic auth encoding", http.StatusUnauthorized)
		return
	}

	// Format is "username:password" - we use JWT as username, password is empty
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) < 1 {
		h.errorResponse(w, "invalid basic auth format", http.StatusUnauthorized)
		return
	}

	originalToken := parts[0]
	if originalToken == "" {
		h.errorResponse(w, "missing credentials", http.StatusUnauthorized)
		return
	}

	// Validate the original JWT
	claims := &RegistryTokenClaims{}
	token, err := jwt.ParseWithClaims(originalToken, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(h.jwtSecret), nil
	})
	if err != nil || !token.Valid {
		h.errorResponse(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// Parse requested scope from query params
	// Format: repository:name:actions (e.g., repository:builds/abc123:push,pull)
	requestedScope := r.URL.Query().Get("scope")
	
	// Build access list from the original token's permissions
	var access []AccessEntry
	
	if requestedScope != "" {
		// Parse the requested scope
		scopeParts := strings.SplitN(requestedScope, ":", 3)
		if len(scopeParts) == 3 && scopeParts[0] == "repository" {
			repoName := scopeParts[1]
			requestedActions := strings.Split(scopeParts[2], ",")
			
			// Check if the original token allows this repo
			allowedActions := h.getAllowedActions(claims, repoName)
			
			// Intersect requested with allowed
			grantedActions := intersect(requestedActions, allowedActions)
			
			if len(grantedActions) > 0 {
				access = append(access, AccessEntry{
					Type:    "repository",
					Name:    repoName,
					Actions: grantedActions,
				})
			}
		}
	} else {
		// No specific scope requested - grant all permissions from original token
		if len(claims.RepoAccess) > 0 {
			for _, perm := range claims.RepoAccess {
				actions := scopeToActions(perm.Scope)
				access = append(access, AccessEntry{
					Type:    "repository",
					Name:    perm.Repo,
					Actions: actions,
				})
			}
		} else {
			// Legacy format
			for _, repo := range claims.Repositories {
				actions := scopeToActions(claims.Scope)
				access = append(access, AccessEntry{
					Type:    "repository",
					Name:    repo,
					Actions: actions,
				})
			}
		}
	}

	// Generate a short-lived access token
	expiresIn := 300 // 5 minutes
	now := time.Now()
	
	accessClaims := &AccessTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    h.service,
			Subject:   claims.BuildID,
			Audience:  jwt.ClaimStrings{h.service},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(expiresIn) * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
		Access: access,
	}

	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	signedToken, err := accessToken.SignedString([]byte(h.jwtSecret))
	if err != nil {
		h.errorResponse(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	// Return token response
	response := TokenResponse{
		Token:       signedToken,
		AccessToken: signedToken,
		ExpiresIn:   expiresIn,
		IssuedAt:    now.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// getAllowedActions returns the actions allowed for a repo based on the claims.
func (h *TokenHandler) getAllowedActions(claims *RegistryTokenClaims, repo string) []string {
	// Check new format first
	if len(claims.RepoAccess) > 0 {
		for _, perm := range claims.RepoAccess {
			if perm.Repo == repo || matchesWildcard(perm.Repo, repo) {
				return scopeToActions(perm.Scope)
			}
		}
	}
	
	// Fall back to legacy format
	for _, allowedRepo := range claims.Repositories {
		if allowedRepo == repo || matchesWildcard(allowedRepo, repo) {
			return scopeToActions(claims.Scope)
		}
	}
	
	return nil
}

// matchesWildcard checks if a pattern matches a repo name.
// Supports prefix matching with /* suffix.
func matchesWildcard(pattern, repo string) bool {
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(repo, prefix+"/")
	}
	return pattern == repo
}

// scopeToActions converts a scope string to action list.
func scopeToActions(scope string) []string {
	switch scope {
	case "push":
		return []string{"pull", "push"}
	case "pull":
		return []string{"pull"}
	default:
		return []string{"pull"}
	}
}

// intersect returns elements common to both slices.
func intersect(a, b []string) []string {
	set := make(map[string]bool)
	for _, v := range b {
		set[v] = true
	}
	var result []string
	for _, v := range a {
		if set[v] {
			result = append(result, v)
		}
	}
	return result
}

func (h *TokenHandler) errorResponse(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry token"`)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// returnAnonymousToken handles anonymous token requests.
// For builds/* paths, we grant access because:
// 1. Build IDs are cryptographically random and hard to guess
// 2. Builds are short-lived (tokens expire quickly)
// 3. BuildKit doesn't send Docker config credentials to the token endpoint
//
// This is a pragmatic solution to work around BuildKit's auth limitations.
// The build ID serves as an implicit authentication factor.
func (h *TokenHandler) returnAnonymousToken(w http.ResponseWriter, r *http.Request) {
	requestedScope := r.URL.Query().Get("scope")
	
	// Parse scope: "repository:builds/abc123:pull,push"
	var access []AccessEntry
	if requestedScope != "" {
		scopeParts := strings.SplitN(requestedScope, ":", 3)
		if len(scopeParts) == 3 && scopeParts[0] == "repository" {
			repoName := scopeParts[1]
			actions := strings.Split(scopeParts[2], ",")
			
			// For builds/* paths, grant the requested access
			// The build ID is random and serves as implicit auth
			if strings.HasPrefix(repoName, "builds/") {
				access = append(access, AccessEntry{
					Type:    "repository",
					Name:    repoName,
					Actions: actions,
				})
			}
			// For cache/* paths, also grant access (cache is scoped per-tenant)
			if strings.HasPrefix(repoName, "cache/") {
				access = append(access, AccessEntry{
					Type:    "repository",
					Name:    repoName,
					Actions: actions,
				})
			}
		}
	}

	expiresIn := 300 // 5 minutes
	now := time.Now()

	accessClaims := &AccessTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    h.service,
			Subject:   "anonymous-builder",
			Audience:  jwt.ClaimStrings{h.service},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(expiresIn) * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
		Access: access,
	}

	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	signedToken, err := accessToken.SignedString([]byte(h.jwtSecret))
	if err != nil {
		h.errorResponse(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	response := TokenResponse{
		Token:       signedToken,
		AccessToken: signedToken,
		ExpiresIn:   expiresIn,
		IssuedAt:    now.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
