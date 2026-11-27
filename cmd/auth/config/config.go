// Package config loads the configuration of hypeman-auth.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/go-viper/mapstructure/v2"
	"github.com/kernel/hypeman/lib/oidcfederation"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// Key names a PEM-encoded key, given inline or as the path of a file.
type Key struct {
	PEM  string `koanf:"pem"`
	File string `koanf:"file"`
}

// Read returns the PEM text of the key.
func (k Key) Read() ([]byte, error) {
	switch {
	case k.PEM != "" && k.File != "":
		return nil, errors.New("give a key as pem or as file, not both")
	case k.PEM != "":
		return []byte(k.PEM), nil
	case k.File != "":
		return os.ReadFile(k.File)
	}
	return nil, errors.New("a key needs pem or file")
}

// ReplayStore says where spent single-use identity tokens are recorded.
type ReplayStore struct {
	// PostgresURL is a PostgreSQL connection string. Every instance of the
	// server must use the same database.
	PostgresURL string `koanf:"postgres_url"`
}

// Config is the configuration of hypeman-auth.
type Config struct {
	// Issuer is the server's issuer URL: the `iss` of its tokens and the URL
	// under which its endpoints are reached.
	Issuer        string `koanf:"issuer"`
	ListenAddress string `koanf:"listen_address"`
	// MetricsListenAddress serves Prometheus metrics when set. It is a
	// separate listener so that it need not be reachable from outside.
	MetricsListenAddress string `koanf:"metrics_listen_address"`

	// SigningKey is the P-256 private key access tokens are signed with.
	SigningKey Key `koanf:"signing_key"`
	// PublishedKeys are P-256 public keys published beside the signing key.
	PublishedKeys []Key `koanf:"published_keys"`

	ReplayStore ReplayStore `koanf:"replay_store"`
	// OIDC holds the trusted identity issuers and the federation rules.
	OIDC oidcfederation.Config `koanf:"oidc"`
	// OtelEndpoint is the URL of an OTLP gRPC collector that metrics are
	// pushed to, such as http://collector:4317. Empty turns the push off.
	OtelEndpoint string `koanf:"otel_endpoint"`
}

// environment maps the environment variables that override the file to their
// keys. Only these are read: the rest of the environment is not configuration.
var environment = map[string]string{
	"ISSUER":                     "issuer",
	"LISTEN_ADDRESS":             "listen_address",
	"METRICS_LISTEN_ADDRESS":     "metrics_listen_address",
	"SIGNING_KEY__PEM":           "signing_key.pem",
	"SIGNING_KEY__FILE":          "signing_key.file",
	"REPLAY_STORE__POSTGRES_URL": "replay_store.postgres_url",
	"OTEL_ENDPOINT":              "otel_endpoint",
}

// structured are the environment variables that hold a JSON document and
// replace their block of the file as a whole. Merging would leave rules from
// the file trusting issuers the environment redefined.
var structured = map[string]string{
	"OIDC":           "oidc",
	"PUBLISHED_KEYS": "published_keys",
}

// Load reads the YAML file at path, if path is not empty, and then the
// environment. A key the configuration does not know is an error wherever it
// appears, and so is a number or boolean written where text belongs: a
// misspelled restriction would otherwise be dropped and leave its rule wider
// than written, and a number written for a text value such as an account ID
// could be rounded on the way in. The file is checked on its own first, so
// that its mistakes show even in blocks the environment then replaces.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(Config{ListenAddress: ":8080"}, "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load default config: %w", err)
	}
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("load config from %s: %w", path, err)
		}
		if _, err := decode(k); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	for name, key := range environment {
		if value := os.Getenv(name); value != "" {
			if err := k.Set(key, value); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	for name, key := range structured {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		var document any
		if err := json.Unmarshal([]byte(value), &document); err != nil {
			return nil, fmt.Errorf("%s must be a JSON document", name)
		}
		k.Delete(key)
		if err := k.Set(key, document); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return decode(k)
}

func decode(k *koanf.Koanf) (*Config, error) {
	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{DecoderConfig: &mapstructure.DecoderConfig{
		TagName:     "koanf",
		ErrorUnused: true,
		Result:      &cfg,
	}}); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}
