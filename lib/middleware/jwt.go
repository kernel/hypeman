package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/onkernel/hypeman/lib/logger"
)

type contextKey string

const userIDKey contextKey = "user_id"

// VerifyJWT validates JWT tokens and extracts user ID
func VerifyJWT(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log := logger.FromContext(r.Context())

			// Extract token from Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				log.WarnContext(r.Context(), "missing authorization header")
				http.Error(w, "Authorization header required", http.StatusUnauthorized)
				return
			}

			// Extract bearer token
			token, err := extractBearerToken(authHeader)
			if err != nil {
				log.WarnContext(r.Context(), "invalid authorization header", "error", err)
				http.Error(w, "Invalid authorization header format", http.StatusUnauthorized)
				return
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
				log.WarnContext(r.Context(), "failed to parse JWT", "error", err)
				http.Error(w, "Invalid token", http.StatusUnauthorized)
				return
			}

			if !parsedToken.Valid {
				log.WarnContext(r.Context(), "invalid JWT token")
				http.Error(w, "Invalid token", http.StatusUnauthorized)
				return
			}

			// Extract user ID from claims (optional - can be extended later)
			var userID string
			if sub, ok := claims["sub"].(string); ok {
				userID = sub
			}

			// Add user ID to context
			ctx := context.WithValue(r.Context(), userIDKey, userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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

// GetUserIDFromContext extracts the user ID from context
func GetUserIDFromContext(ctx context.Context) string {
	if userID, ok := ctx.Value(userIDKey).(string); ok {
		return userID
	}
	return ""
}

