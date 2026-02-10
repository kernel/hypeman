// Package registry implements token authentication for OCI Distribution registries.
package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/logger"
)

// TokenResponse is the response from the /v2/token endpoint per Docker Registry Token spec.
// See: https://distribution.github.io/distribution/spec/auth/token/
type TokenResponse struct {
	// Token is the bearer token to use for registry requests
	Token string `json:"token"`

	// AccessToken is an alias for Token (some clients expect this)
	AccessToken string `json:"access_token,omitempty"`

	// ExpiresIn is the lifetime of the token in seconds
	ExpiresIn int `json:"expires_in,omitempty"`

	// IssuedAt is the time the token was issued (RFC3339)
	IssuedAt string `json:"issued_at,omitempty"`
}

// TokenError is returned when token authentication fails.
type TokenError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TokenErrorResponse wraps token errors.
type TokenErrorResponse struct {
	Errors []TokenError `json:"errors"`
}

// TokenHandler handles /v2/token requests implementing Docker Registry Token Authentication.
// This endpoint is called by Docker/BuildKit clients after receiving a 401 with WWW-Authenticate.
type TokenHandler struct {
	jwtSecret string
}

// NewTokenHandler creates a new token endpoint handler.
// All clients must provide explicit credentials (Basic or Bearer auth with JWT).
func NewTokenHandler(jwtSecret string) *TokenHandler {
	return &TokenHandler{
		jwtSecret: jwtSecret,
	}
}

// ServeHTTP handles GET /v2/token requests.
// Query parameters:
//   - scope: repository:name:actions (e.g., "repository:builds/abc123:push,pull")
//   - service: the registry service name (optional)
//
// Authentication:
//   - Basic auth: JWT as username (legacy) or password (identitytoken format)
//   - Bearer auth: the JWT token directly
func (h *TokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := logger.FromContext(r.Context())

	// Parse scope parameter
	scope := r.URL.Query().Get("scope")

	// Try to authenticate
	token, authMethod := h.extractToken(r)

	if token != "" {
		// Validate the JWT
		claims, err := h.validateJWT(token)
		if err != nil {
			log.DebugContext(r.Context(), "token validation failed", "error", err, "auth_method", authMethod)
			h.writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid credentials")
			return
		}

		// Check if requested scope is allowed by the token.
		// If not, still return a valid token — the subsequent manifest request
		// will get a 404 (not found) instead of 403. This is critical for BuildKit
		// mirror fallback: a 403 on the token endpoint is treated as a hard auth
		// failure and prevents fallback to upstream registries like Docker Hub.
		if scope != "" {
			repo, actions := parseScope(scope)
			if repo != "" && !h.isScopeAllowed(claims, repo, actions) {
				log.DebugContext(r.Context(), "scope not in token, returning token anyway for mirror fallback",
					"requested_repo", repo,
					"requested_actions", actions)
			}
		}

		// Return the same token as a bearer token
		// This is valid because our tokens are already bearer tokens
		log.DebugContext(r.Context(), "token authenticated successfully",
			"auth_method", authMethod,
			"subject", claims["sub"],
			"scope", scope)
		h.writeToken(w, token)
		return
	}

	// IP-based fallback authentication has been removed.
	// All clients must provide explicit credentials (Basic or Bearer auth with JWT).
	// Builder VMs receive their registry token via the build config and should
	// pass it via Basic auth (token as username, empty password).

	// No valid authentication
	log.DebugContext(r.Context(), "token request without valid auth",
		"remote_addr", r.RemoteAddr,
		"has_auth_header", r.Header.Get("Authorization") != "")
	h.writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

// extractToken attempts to extract a JWT from the request.
// Supports both Basic auth (JWT as username) and Bearer auth.
func (h *TokenHandler) extractToken(r *http.Request) (string, string) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", ""
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return "", ""
	}

	scheme := strings.ToLower(parts[0])
	switch scheme {
	case "bearer":
		return parts[1], "bearer"
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return "", ""
		}
		// Format is username:password
		// JWT can be in username (our auth field format) OR password (identitytoken format)
		// BuildKit sends identitytoken as password with empty username
		credentials := strings.SplitN(string(decoded), ":", 2)
		if len(credentials) == 0 {
			return "", ""
		}
		// Try username first (our auth field format: "jwt:")
		if credentials[0] != "" {
			return credentials[0], "basic"
		}
		// Fall back to password (identitytoken format: ":jwt")
		if len(credentials) > 1 && credentials[1] != "" {
			return credentials[1], "basic"
		}
		return "", ""
	}

	return "", ""
}

// validateJWT parses and validates a JWT token.
func (h *TokenHandler) validateJWT(tokenString string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(h.jwtSecret), nil
	})

	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}

// isScopeAllowed checks if the requested scope is allowed by the token claims.
func (h *TokenHandler) isScopeAllowed(claims jwt.MapClaims, repo string, actions []string) bool {
	// Check repo_access (new format) first
	if repoAccess, ok := claims["repo_access"].([]interface{}); ok {
		for _, ra := range repoAccess {
			if perm, ok := ra.(map[string]interface{}); ok {
				if perm["repo"] == repo {
					permScope, _ := perm["scope"].(string)
					return h.scopeAllowsActions(permScope, actions)
				}
			}
		}
		return false
	}

	// Fall back to legacy repos/scope format
	if repos, ok := claims["repos"].([]interface{}); ok {
		for _, r := range repos {
			if r == repo {
				scope, _ := claims["scope"].(string)
				return h.scopeAllowsActions(scope, actions)
			}
		}
	}

	return false
}

// scopeAllowsActions checks if a scope (push/pull) allows the requested actions.
func (h *TokenHandler) scopeAllowsActions(scope string, actions []string) bool {
	for _, action := range actions {
		switch action {
		case "push":
			if scope != "push" {
				return false
			}
		case "pull":
			if scope != "push" && scope != "pull" {
				return false
			}
		}
	}
	return true
}

// parseScope parses a Docker registry scope string.
// Format: "repository:name:actions" where actions is comma-separated.
// Example: "repository:builds/abc123:push,pull"
func parseScope(scope string) (repo string, actions []string) {
	parts := strings.SplitN(scope, ":", 3)
	if len(parts) < 3 || parts[0] != "repository" {
		return "", nil
	}
	repo = parts[1]
	actions = strings.Split(parts[2], ",")
	return repo, actions
}

// writeToken writes a successful token response.
func (h *TokenHandler) writeToken(w http.ResponseWriter, token string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	resp := TokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   300, // 5 minutes
		IssuedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	json.NewEncoder(w).Encode(resp)
}

// writeError writes an error response.
// For 401 responses, includes WWW-Authenticate header to tell clients how to authenticate.
func (h *TokenHandler) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")

	// For 401 Unauthorized, include WWW-Authenticate header
	// This tells clients (like BuildKit) to retry with Basic auth credentials
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="hypeman"`)
	}

	w.WriteHeader(status)

	resp := TokenErrorResponse{
		Errors: []TokenError{{Code: code, Message: message}},
	}

	json.NewEncoder(w).Encode(resp)
}
