package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/golang-jwt/jwt/v5"
	"github.com/onkernel/hypeman/lib/logger"
)

type contextKey string

const userIDKey contextKey = "user_id"

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

// GetUserIDFromContext extracts the user ID from context
func GetUserIDFromContext(ctx context.Context) string {
	if userID, ok := ctx.Value(userIDKey).(string); ok {
		return userID
	}
	return ""
}

