package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// getJWTSecret retrieves the JWT secret with the following precedence:
// 1. JWT_SECRET environment variable
// 2. jwt_secret from CONFIG_PATH config file (if set)
// 3. jwt_secret from default config.yaml paths (same as hypeman-api)
func getJWTSecret() string {
	// 1. Check environment variable first (highest precedence)
	if s := os.Getenv("JWT_SECRET"); s != "" {
		return s
	}

	// 2. Try CONFIG_PATH first (same as hypeman-api), then default paths
	k := koanf.New(".")
	if configPath := os.Getenv("CONFIG_PATH"); configPath != "" {
		if err := k.Load(file.Provider(configPath), yaml.Parser()); err == nil {
			if s := k.String("jwt_secret"); s != "" {
				return s
			}
		}
	}

	// 3. Try default config file paths
	for _, path := range config.GetDefaultConfigPaths() {
		if err := k.Load(file.Provider(path), yaml.Parser()); err == nil {
			if s := k.String("jwt_secret"); s != "" {
				return s
			}
		}
	}

	return ""
}

func main() {
	userID := flag.String("user-id", "test-user", "User ID to include in the JWT token")
	duration := flag.Duration("duration", 24*time.Hour, "Token validity duration (e.g., 24h, 720h, 8760h)")
	flag.Parse()

	jwtSecret := getJWTSecret()
	if jwtSecret == "" {
		fmt.Fprintf(os.Stderr, "Error: JWT_SECRET not found.\n")
		fmt.Fprintf(os.Stderr, "Set JWT_SECRET environment variable, set CONFIG_PATH, or ensure jwt_secret is configured in:\n")
		for _, path := range config.GetDefaultConfigPaths() {
			fmt.Fprintf(os.Stderr, "  - %s\n", path)
		}
		os.Exit(1)
	}

	claims := jwt.MapClaims{
		"sub": *userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(*duration).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(jwtSecret))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating token: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(tokenString)
}
