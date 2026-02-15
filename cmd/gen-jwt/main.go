package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// getServerConfigPaths returns the default config file paths to search for jwt_secret.
// This matches the paths used by hypeman-api.
func getServerConfigPaths() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return []string{
			filepath.Join(home, ".config", "hypeman", "config.yaml"),
		}
	}
	// Linux: check /etc first, then user config
	return []string{
		"/etc/hypeman/config.yaml",
		filepath.Join(home, ".config", "hypeman", "config.yaml"),
	}
}

// getJWTSecret retrieves the JWT secret with the following precedence:
// 1. JWT_SECRET environment variable
// 2. jwt_secret from config.yaml files
func getJWTSecret() string {
	// 1. Check environment variable first (highest precedence)
	if s := os.Getenv("JWT_SECRET"); s != "" {
		return s
	}

	// 2. Try to read from config files
	k := koanf.New(".")
	for _, path := range getServerConfigPaths() {
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
		fmt.Fprintf(os.Stderr, "Set JWT_SECRET environment variable or ensure jwt_secret is configured in:\n")
		for _, path := range getServerConfigPaths() {
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
