package cmd

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/urfave/cli/v3"
)

// CLIConfig holds CLI configuration loaded from cli.yaml
type CLIConfig struct {
	BaseURL string `koanf:"base_url"`
	APIKey  string `koanf:"api_key"`
}

// getCLIConfigPath returns the path to the CLI config file.
// The CLI uses ~/.config/hypeman/cli.yaml on all platforms.
func getCLIConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "hypeman", "cli.yaml")
}

// loadCLIConfig loads CLI configuration from the config file, then
// overlays HYPEMAN_-prefixed environment variables (highest precedence).
// HYPEMAN_BASE_URL -> base_url, HYPEMAN_API_KEY -> api_key.
// Returns an empty config if the file doesn't exist or can't be parsed.
func loadCLIConfig() *CLIConfig {
	cfg := &CLIConfig{}
	k := koanf.New(".")

	configPath := getCLIConfigPath()
	if configPath != "" {
		_ = k.Load(file.Provider(configPath), yaml.Parser())
	}

	// Overlay HYPEMAN_-prefixed env vars: HYPEMAN_BASE_URL -> base_url
	_ = k.Load(env.ProviderWithValue("HYPEMAN_", ".", func(key string, value string) (string, interface{}) {
		if value == "" {
			return "", nil
		}
		return strings.ToLower(strings.TrimPrefix(key, "HYPEMAN_")), value
	}), nil)

	_ = k.Unmarshal("", cfg)
	return cfg
}

// resolveBaseURL returns the effective base URL with precedence:
// CLI flag > HYPEMAN_BASE_URL env > config file > default.
func resolveBaseURL(cmd *cli.Command) string {
	if u := cmd.Root().String("base-url"); u != "" {
		return u
	}
	cfg := loadCLIConfig()
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return "http://localhost:8080"
}

// resolveAPIKey returns the effective API key with precedence:
// HYPEMAN_API_KEY env > config file.
func resolveAPIKey() string {
	cfg := loadCLIConfig()
	return cfg.APIKey
}
