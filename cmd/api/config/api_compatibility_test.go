package config

import (
	"strings"
	"testing"
)

func TestValidateAPIHTTPSPortsUnconfigured(t *testing.T) {
	for _, ports := range [][]int{nil, {}} {
		for _, tls := range []bool{false, true} {
			for _, hostname := range []string{"", "api.example.com"} {
				cfg := defaultConfig()
				cfg.API.HTTPSPorts = ports
				cfg.API.TLS = tls
				cfg.API.Hostname = hostname
				if err := cfg.Validate(); err != nil {
					t.Fatalf("unconfigured ports=%v tls=%v hostname=%q: %v", ports, tls, hostname, err)
				}
			}
		}
	}
}

func TestValidateAPIHTTPSPortConflicts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*Config)
		wantError string
	}{
		{"current configuration", func(c *Config) {
			c.API.HTTPSPorts = nil
		}, ""},
		{"explicit current port", func(c *Config) {
			c.API.HTTPSPorts = []int{443}
		}, ""},
		{"migration configuration", func(c *Config) {}, ""},
		{"default backend collision", func(c *Config) {
			c.Port = "4973"
		}, "port 4973 conflicts with backend port"},
		{"backend collision", func(c *Config) {
			c.API.HTTPSPorts = []int{8080}
		}, "port 8080 conflicts with backend port"},
		{"metrics collision", func(c *Config) {
			c.API.HTTPSPorts = []int{9464}
		}, "conflicts with metrics.port"},
		{"admin collision", func(c *Config) {
			c.API.HTTPSPorts = []int{2019}
		}, "conflicts with caddy.admin_port"},
		{"random admin port", func(c *Config) {
			c.Caddy.AdminPort = 0
		}, ""},
		{"disjoint backend bind", func(c *Config) {
			c.Caddy.ListenAddress = "192.0.2.1"
			c.ListenAddress = "127.0.0.1"
			c.Port = "4973"
		}, ""},
		{"disjoint metrics bind", func(c *Config) {
			c.Caddy.ListenAddress = "192.0.2.1"
			c.API.HTTPSPorts = []int{9464}
		}, ""},
		{"disjoint admin bind", func(c *Config) {
			c.Caddy.ListenAddress = "192.0.2.1"
			c.API.HTTPSPorts = []int{2019}
		}, ""},
		{"disabled API ingress", func(c *Config) {
			c.API.Hostname = ""
			c.Port = "4973"
		}, ""},
		{"TLS disabled", func(c *Config) {
			c.API.TLS = false
		}, "requires api.tls"},
		{"invalid backend port", func(c *Config) {
			c.Port = "invalid"
		}, "invalid backend port"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Port = "8080"
			cfg.API.Hostname = "api.example.com"
			cfg.API.HTTPSPorts = []int{443, 4973}
			cfg.Caddy.AdminPort = 2019
			tt.configure(cfg)
			err := cfg.Validate()
			if tt.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func TestBindAddressesOverlap(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"", "127.0.0.1", true},
		{"127.0.0.1", "", true},
		{"0.0.0.0", "127.0.0.1", true},
		{"::", "::1", true},
		{"::1", "::", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.1", "::ffff:127.0.0.1", true},
		{"localhost", "127.0.0.1", true},
		{"127.0.0.1", "localhost", true},
		{"192.0.2.1", "127.0.0.1", false},
		{"2001:db8::1", "::1", false},
	} {
		t.Run(tt.a+"/"+tt.b, func(t *testing.T) {
			if got := bindAddressesOverlap(tt.a, tt.b); got != tt.want {
				t.Fatalf("overlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
