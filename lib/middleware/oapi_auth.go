package middleware

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/logger"
)

type contextKey string

const userIDKey contextKey = "user_id"

// registryPathPattern matches /v2/{repository}/... paths
var registryPathPattern = regexp.MustCompile(`^/v2/([^/]+(?:/[^/]+)?)/`)

// RegistryTokenClaims contains the claims for a scoped registry access token.
// This mirrors the type in lib/builds/registry_token.go to avoid circular imports.
type RegistryTokenClaims struct {
	jwt.RegisteredClaims
	BuildID      string   `json:"build_id"`
	Repositories []string `json:"repos"`
	Scope        string   `json:"scope"`
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
		credentials := strings.SplitN(string(decoded), ":", 2)
		if len(credentials) == 0 {
			return "", "", fmt.Errorf("invalid basic auth format")
		}
		// The JWT is the username part
		return credentials[0], "basic", nil
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

// isInternalVMRequest checks if the request is from an internal VM network
// This is used as a fallback for builder VMs that don't have token auth yet.
//
// SECURITY: We only trust RemoteAddr, not X-Real-IP or X-Forwarded-For headers,
// as those can be spoofed by attackers to bypass authentication.
func isInternalVMRequest(r *http.Request) bool {
	// Use only RemoteAddr - never trust client-supplied headers for auth decisions
	ip := r.RemoteAddr

	// RemoteAddr is "IP:port" format, extract just the IP
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}

	// Check if it's from the VM network (10.100.x.x or 10.102.x.x)
	return strings.HasPrefix(ip, "10.100.") || strings.HasPrefix(ip, "10.102.")
}

// extractRepoFromPath extracts the repository name from a registry path.
// e.g., "/v2/builds/abc123/manifests/latest" -> "builds/abc123"
func extractRepoFromPath(path string) string {
	matches := registryPathPattern.FindStringSubmatch(path)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// isWriteOperation returns true if the HTTP method implies a write operation
func isWriteOperation(method string) bool {
	return method == http.MethodPut || method == http.MethodPost || method == http.MethodPatch || method == http.MethodDelete
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

	// Check if this is a registry token (has repos claim)
	if len(claims.Repositories) == 0 {
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
	for _, allowedRepo := range claims.Repositories {
		if allowedRepo == repo {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("repository %s not allowed by token", repo)
	}

	// Check scope for write operations
	if isWriteOperation(method) && claims.Scope != "push" {
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
				if authHeader != "" {
					// Try to extract token (supports both Bearer and Basic auth)
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
					} else {
						log.DebugContext(r.Context(), "failed to extract token", "error", err)
					}
				}

				// Fallback: Allow internal VM network (10.102.x.x) for registry pushes
				// This is a transitional fallback for older builder images without token auth
				if isInternalVMRequest(r) {
					log.DebugContext(r.Context(), "allowing internal VM request via IP fallback (deprecated)",
						"remote_addr", r.RemoteAddr,
						"path", r.URL.Path)
					ctx := context.WithValue(r.Context(), userIDKey, "internal-builder-legacy")
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}

				// Registry auth failed
				log.DebugContext(r.Context(), "registry request unauthorized", "remote_addr", r.RemoteAddr)
				OapiErrorHandler(w, "registry authentication required", http.StatusUnauthorized)
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
