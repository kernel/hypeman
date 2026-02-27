package middleware

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	v2 "github.com/docker/distribution/registry/api/v2"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/mux"
	"github.com/kernel/hypeman/lib/logger"
)

// errRepoNotAllowed is returned when a valid token doesn't have access to the requested repository.
var errRepoNotAllowed = errors.New("repository not allowed by token")

type contextKey string

const userIDKey contextKey = "user_id"

// registryRouter is the OCI Distribution API router from docker/distribution.
// It properly parses repository names (which can contain slashes) from /v2/ paths.
var registryRouter = v2.Router()

// RegistryTokenClaims contains the claims for a scoped registry access token.
// This mirrors the type in lib/builds/registry_token.go to avoid circular imports.
// RepoPermission defines access permissions for a specific repository
type RepoPermission struct {
	Repo  string `json:"repo"`
	Scope string `json:"scope"`
}

type RegistryTokenClaims struct {
	jwt.RegisteredClaims
	BuildID string `json:"build_id"`

	// RepoAccess is the new format - array of repo permissions
	RepoAccess []RepoPermission `json:"repo_access,omitempty"`

	// Legacy format fields (kept for backward compat)
	Repositories []string `json:"repos,omitempty"`
	Scope        string   `json:"scope,omitempty"`
}

// OapiAuthenticationFunc creates an AuthenticationFunc compatible with nethttp-middleware
// that validates JWT bearer tokens for endpoints with security requirements.
func OapiAuthenticationFunc(jwtSecret string) openapi3filter.AuthenticationFunc {
	return func(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
		log := logger.FromContext(ctx)

		// If no security requirements, allow the request
		if input.SecurityScheme == nil {
			return nil
		}

		// Only handle bearer auth
		if input.SecurityScheme.Type != "http" || input.SecurityScheme.Scheme != "bearer" {
			return fmt.Errorf("unsupported security scheme: %s", input.SecurityScheme.Type)
		}

		// Extract token from Authorization header
		authHeader := input.RequestValidationInput.Request.Header.Get("Authorization")
		if authHeader == "" {
			log.DebugContext(ctx, "missing authorization header")
			return fmt.Errorf("authorization header required")
		}

		// Extract bearer token
		token, err := extractBearerToken(authHeader)
		if err != nil {
			log.DebugContext(ctx, "invalid authorization header", "error", err)
			return fmt.Errorf("invalid authorization header format")
		}

		// Parse and validate JWT
		claims := jwt.MapClaims{}
		parsedToken, err := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (interface{}, error) {
			// Validate signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return []byte(jwtSecret), nil
		})

		if err != nil {
			log.DebugContext(ctx, "failed to parse JWT", "error", err)
			return fmt.Errorf("invalid token")
		}

		if !parsedToken.Valid {
			log.DebugContext(ctx, "invalid JWT token")
			return fmt.Errorf("invalid token")
		}

		// Reject registry tokens - they should not be used for API authentication.
		// Registry tokens have specific claims (repos, scope, build_id) that user tokens don't have.
		if _, hasRepos := claims["repos"]; hasRepos {
			log.DebugContext(ctx, "rejected registry token used for API auth")
			return fmt.Errorf("invalid token type")
		}
		if _, hasScope := claims["scope"]; hasScope {
			log.DebugContext(ctx, "rejected registry token used for API auth")
			return fmt.Errorf("invalid token type")
		}
		if _, hasBuildID := claims["build_id"]; hasBuildID {
			log.DebugContext(ctx, "rejected registry token used for API auth")
			return fmt.Errorf("invalid token type")
		}

		// Extract user ID from claims and add to context
		var userID string
		if sub, ok := claims["sub"].(string); ok {
			userID = sub
		}

		// Update the context with user ID
		newCtx := context.WithValue(ctx, userIDKey, userID)

		// Update the request with the new context
		*input.RequestValidationInput.Request = *input.RequestValidationInput.Request.WithContext(newCtx)

		return nil
	}
}

// OapiErrorHandler creates a custom error handler for nethttp-middleware
// that returns consistent error responses.
func OapiErrorHandler(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	// Return a simple JSON error response matching our Error schema
	fmt.Fprintf(w, `{"code":"%s","message":"%s"}`,
		http.StatusText(statusCode),
		message)
}

// extractBearerToken extracts the token from "Bearer <token>" format
func extractBearerToken(authHeader string) (string, error) {
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid authorization header format")
	}

	scheme := strings.ToLower(parts[0])
	if scheme != "bearer" {
		return "", fmt.Errorf("unsupported authorization scheme: %s", scheme)
	}

	return parts[1], nil
}

// extractTokenFromAuth extracts a JWT token from either Bearer or Basic auth headers.
// For Bearer: returns the token directly
// For Basic: decodes base64 and returns the username part (BuildKit sends JWT as username)
func extractTokenFromAuth(authHeader string) (string, string, error) {
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid authorization header format")
	}

	scheme := strings.ToLower(parts[0])
	switch scheme {
	case "bearer":
		return parts[1], "bearer", nil
	case "basic":
		// Decode base64
		decoded, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return "", "", fmt.Errorf("invalid basic auth encoding: %w", err)
		}
		// Split on colon to get username:password
		// JWT can be in username (our auth field format) OR password (identitytoken format)
		// BuildKit sends identitytoken as password with empty username
		credentials := strings.SplitN(string(decoded), ":", 2)
		if len(credentials) == 0 {
			return "", "", fmt.Errorf("invalid basic auth format")
		}
		// Try username first (our auth field format: "jwt:")
		if credentials[0] != "" {
			return credentials[0], "basic", nil
		}
		// Fall back to password (identitytoken format: ":jwt")
		if len(credentials) > 1 && credentials[1] != "" {
			return credentials[1], "basic", nil
		}
		return "", "", fmt.Errorf("empty credentials")
	default:
		return "", "", fmt.Errorf("unsupported authorization scheme: %s", scheme)
	}
}

// GetUserIDFromContext extracts the user ID from context
func GetUserIDFromContext(ctx context.Context) string {
	if userID, ok := ctx.Value(userIDKey).(string); ok {
		return userID
	}
	return ""
}

// isRegistryPath checks if the request is for the OCI registry endpoints (/v2/...)
func isRegistryPath(path string) bool {
	return strings.HasPrefix(path, "/v2/")
}

// isTokenEndpoint checks if the request is for the /v2/token endpoint
func isTokenEndpoint(path string) bool {
	return path == "/v2/token" || path == "/v2/token/"
}

// extractRepoFromPath extracts the repository name from a registry path.
// Uses the docker/distribution router which properly handles repository names
// that can contain slashes (e.g., "builds/abc123" from "/v2/builds/abc123/manifests/latest").
func extractRepoFromPath(path string) string {
	// Create a minimal request for route matching
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return ""
	}

	var match mux.RouteMatch
	if registryRouter.Match(req, &match) {
		if name, ok := match.Vars["name"]; ok {
			return name
		}
	}
	return ""
}

// isWriteOperation returns true if the HTTP method implies a write operation
func isWriteOperation(method string) bool {
	return method == http.MethodPut || method == http.MethodPost || method == http.MethodPatch || method == http.MethodDelete
}

// writeRegistryUnauthorized writes a 401 response with proper WWW-Authenticate header.
// We use Bearer token flow because:
// 1. BuildKit expects to receive a Bearer challenge with a token endpoint URL
// 2. BuildKit will call /v2/token with Basic auth (JWT from docker config.json as username)
// 3. Our token handler validates the JWT and returns it as a Bearer token
// 4. BuildKit then retries the original request with the Bearer token
func writeRegistryUnauthorized(w http.ResponseWriter, r *http.Request) {
	// Build the token endpoint URL from the request
	// Detect scheme from the incoming request to support both HTTP and HTTPS registries
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	tokenURL := fmt.Sprintf("%s://%s/v2/token", scheme, host)

	// Use Bearer challenge pointing to our token endpoint
	challenge := fmt.Sprintf(`Bearer realm="%s",service="hypeman"`, tokenURL)
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)

	// Return error in OCI Distribution format
	fmt.Fprintf(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
}

// validateRegistryToken validates a registry-scoped JWT token and checks repository access.
// Returns the claims if valid, nil otherwise.
func validateRegistryToken(tokenString, jwtSecret, requestPath, method string) (*RegistryTokenClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &RegistryTokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(jwtSecret), nil
	})

	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(*RegistryTokenClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	// Check if this is a registry token (has repo_access or repos claim)
	hasRepoAccess := len(claims.RepoAccess) > 0
	hasLegacyRepos := len(claims.Repositories) > 0
	if !hasRepoAccess && !hasLegacyRepos {
		return nil, fmt.Errorf("not a registry token")
	}

	// Extract repository from request path
	repo := extractRepoFromPath(requestPath)
	if repo == "" {
		// Allow /v2/ (base path check) without repo validation
		if requestPath == "/v2/" || requestPath == "/v2" {
			return claims, nil
		}
		return nil, fmt.Errorf("could not extract repository from path")
	}

	// Check if the repository is allowed by the token
	allowed := false
	scope := ""

	// Check new format (repo_access) first
	if hasRepoAccess {
		for _, perm := range claims.RepoAccess {
			if perm.Repo == repo {
				allowed = true
				scope = perm.Scope
				break
			}
		}
	} else {
		// Fall back to legacy format
		for _, allowedRepo := range claims.Repositories {
			if allowedRepo == repo {
				allowed = true
				scope = claims.Scope
				break
			}
		}
	}

	if !allowed {
		return nil, fmt.Errorf("%w: %s", errRepoNotAllowed, repo)
	}

	// Check scope for write operations
	if isWriteOperation(method) && scope != "push" {
		return nil, fmt.Errorf("token does not allow write operations")
	}

	return claims, nil
}

// JwtAuth creates a chi middleware that validates JWT bearer tokens
func JwtAuth(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log := logger.FromContext(r.Context())

			// Extract token from Authorization header
			authHeader := r.Header.Get("Authorization")

			// For registry paths, handle specially to support both Bearer and Basic auth
			if isRegistryPath(r.URL.Path) {
				// Allow /v2/token endpoint through without auth - it handles its own auth
				// This implements the Docker Registry Token Authentication flow
				if isTokenEndpoint(r.URL.Path) {
					log.DebugContext(r.Context(), "allowing token endpoint request through",
						"remote_addr", r.RemoteAddr)
					next.ServeHTTP(w, r)
					return
				}

				if authHeader != "" {
					// Try to extract token (supports both Bearer and Basic auth)
					log.InfoContext(r.Context(), "registry request with auth header",
						"path", r.URL.Path,
						"method", r.Method,
						"auth_type", strings.Split(authHeader, " ")[0],
						"remote_addr", r.RemoteAddr)
					token, authType, err := extractTokenFromAuth(authHeader)
					if err == nil {
						log.DebugContext(r.Context(), "extracted token for registry request", "auth_type", authType)

						// Try to validate as a registry-scoped token
						registryClaims, err := validateRegistryToken(token, jwtSecret, r.URL.Path, r.Method)
						if err == nil {
							// Valid registry token - set build ID as user for audit trail
							log.DebugContext(r.Context(), "registry token validated",
								"build_id", registryClaims.BuildID,
								"repos", registryClaims.Repositories,
								"scope", registryClaims.Scope)
							ctx := context.WithValue(r.Context(), userIDKey, "builder-"+registryClaims.BuildID)
							next.ServeHTTP(w, r.WithContext(ctx))
							return
						}
						log.DebugContext(r.Context(), "registry token validation failed", "error", err)

						// For read operations (GET/HEAD), if the token is valid but the
						// repo isn't in the allowed list, return 502 Bad Gateway.
						// BuildKit treats 5xx from a mirror as "mirror unavailable" and
						// falls back to the upstream registry (Docker Hub). A 404 would
						// be treated as "image doesn't exist" with no fallback.
						if errors.Is(err, errRepoNotAllowed) && !isWriteOperation(r.Method) {
							log.DebugContext(r.Context(), "returning 502 for mirror fallback",
								"path", r.URL.Path)
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadGateway)
							fmt.Fprintf(w, `{"errors":[{"code":"UNAVAILABLE","message":"image not mirrored"}]}`)
							return
						}
					} else {
						log.DebugContext(r.Context(), "failed to extract token", "error", err)
					}
				}

				// Registry auth failed - return 401 with WWW-Authenticate header
				// This tells clients (like BuildKit) where to get a token
				if authHeader == "" {
					log.InfoContext(r.Context(), "registry request WITHOUT auth header",
						"path", r.URL.Path,
						"method", r.Method,
						"remote_addr", r.RemoteAddr)
				}
				writeRegistryUnauthorized(w, r)
				return
			}

			// For non-registry paths, require Bearer token
			if authHeader == "" {
				log.DebugContext(r.Context(), "missing authorization header")
				OapiErrorHandler(w, "authorization header required", http.StatusUnauthorized)
				return
			}

			// Extract bearer token
			token, err := extractBearerToken(authHeader)
			if err != nil {
				log.DebugContext(r.Context(), "invalid authorization header", "error", err)
				OapiErrorHandler(w, "invalid authorization header format", http.StatusUnauthorized)
				return
			}

			// Parse and validate as regular user JWT
			claims := jwt.MapClaims{}
			parsedToken, err := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (interface{}, error) {
				// Validate signing method
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
				}
				return []byte(jwtSecret), nil
			})

			if err != nil {
				log.DebugContext(r.Context(), "failed to parse JWT", "error", err)
				OapiErrorHandler(w, "invalid token", http.StatusUnauthorized)
				return
			}

			if !parsedToken.Valid {
				log.DebugContext(r.Context(), "invalid JWT token")
				OapiErrorHandler(w, "invalid token", http.StatusUnauthorized)
				return
			}

			// Reject registry tokens - they should not be used for API authentication.
			// Registry tokens have specific claims that user tokens don't have.
			// This provides defense-in-depth even though BuildKit isolates build containers.
			if _, hasRepos := claims["repos"]; hasRepos {
				log.DebugContext(r.Context(), "rejected registry token used for API auth")
				OapiErrorHandler(w, "invalid token type", http.StatusUnauthorized)
				return
			}
			if _, hasScope := claims["scope"]; hasScope {
				log.DebugContext(r.Context(), "rejected registry token used for API auth")
				OapiErrorHandler(w, "invalid token type", http.StatusUnauthorized)
				return
			}
			if _, hasBuildID := claims["build_id"]; hasBuildID {
				log.DebugContext(r.Context(), "rejected registry token used for API auth")
				OapiErrorHandler(w, "invalid token type", http.StatusUnauthorized)
				return
			}
			// Also reject tokens with "builder-" prefix in subject as an extra safeguard
			if sub, ok := claims["sub"].(string); ok && strings.HasPrefix(sub, "builder-") {
				log.DebugContext(r.Context(), "rejected builder token used for API auth", "sub", sub)
				OapiErrorHandler(w, "invalid token type", http.StatusUnauthorized)
				return
			}

			// Extract user ID from claims and add to context
			var userID string
			if sub, ok := claims["sub"].(string); ok {
				userID = sub
			}

			// Update the context with user ID
			newCtx := context.WithValue(r.Context(), userIDKey, userID)

			// Call next handler with updated context
			next.ServeHTTP(w, r.WithContext(newCtx))
		})
	}
}
