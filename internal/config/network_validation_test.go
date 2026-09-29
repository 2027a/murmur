package config

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"git.2027a.net/2027a/murmur/internal/errs"
)

func TestProviderLiteralZoneValidation(t *testing.T) {
	for _, kind := range []string{"STT primary", "STT fallback", "STT inactive", "cleanup active", "cleanup inactive"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name, host, zone string
				trusted          []string
				invalid          bool
			}{
				{name: "trusted CGNAT", host: "100.64.0.10", zone: "lan", trusted: []string{"100.64.0.0/10"}},
				{name: "CGNAT without trust", host: "100.64.0.10", zone: "lan", invalid: true},
				{name: "outside trusted network", host: "100.128.0.1", zone: "lan", trusted: []string{"100.64.0.0/10"}, invalid: true},
				{name: "trusted IPv4", host: "198.51.100.42", zone: "lan", trusted: []string{"198.51.100.0/24"}},
				{name: "untrusted IPv4", host: "203.0.113.42", zone: "lan", trusted: []string{"198.51.100.0/24"}, invalid: true},
				{name: "private IPv4", host: "192.168.1.42", zone: "lan"},
				{name: "link-local IPv4", host: "169.254.1.42", zone: "lan"},
				{name: "private IPv6", host: "fd00::42", zone: "lan"},
				{name: "link-local IPv6", host: "fe80::42", zone: "lan"},
				{name: "trusted IPv6", host: "2001:db8::42", zone: "lan", trusted: []string{"2001:db8::/32"}},
				{name: "untrusted IPv6", host: "2001:db9::42", zone: "lan", trusted: []string{"2001:db8::/32"}, invalid: true},
				{name: "mapped trusted IPv4", host: "::ffff:100.64.0.10", zone: "lan", trusted: []string{"100.64.0.0/10"}},
				{name: "loopback allowed below LAN", host: "127.0.0.1", zone: "lan"},
				{name: "IPv6 loopback below LAN", host: "::1", zone: "lan"},
				{name: "local loopback", host: "127.0.0.1", zone: "local"},
				{name: "local IPv6 loopback", host: "::1", zone: "local"},
				{name: "trusted network is not local", host: "100.64.0.10", zone: "local", trusted: []string{"100.64.0.0/10"}, invalid: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cfg := addressValidationConfig(kind, tc.host, tc.zone)
					cfg.Policy.TrustedNetworks = tc.trusted
					err := Validate(cfg)
					if tc.invalid {
						if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "base_url") {
							t.Fatalf("error=%v, want invalid literal address configuration", err)
						}
					} else if err != nil {
						t.Fatalf("valid address rejected: %v", err)
					}
				})
			}
		})
	}
}

func TestTrustedProviderStillRequiresAllowedZone(t *testing.T) {
	for _, kind := range []string{"STT primary", "STT fallback", "cleanup active"} {
		t.Run(kind, func(t *testing.T) {
			cfg := addressValidationConfig(kind, "100.64.0.10", "lan")
			cfg.Policy.TrustedNetworks = []string{"100.64.0.0/10"}
			cfg.Policy.STTZones = []string{"local"}
			cfg.Policy.CleanupZones = []string{"local"}
			if err := Validate(cfg); errs.Classify(err) != errs.PolicyBlocked {
				t.Fatalf("error=%v, want policy_blocked", err)
			}
		})
	}
}

func TestTrustedNetworkParsingPrecedesProviderValidation(t *testing.T) {
	cfg := addressValidationConfig("STT primary", "100.64.0.10", "lan")
	cfg.Policy.TrustedNetworks = []string{"not-a-CIDR"}
	if err := Validate(cfg); errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "trusted_networks") {
		t.Fatalf("error=%v, want invalid trusted_networks", err)
	}
}

func TestPreflightDoesNotResolveProviderHostnames(t *testing.T) {
	original := net.DefaultResolver
	var lookups atomic.Int32
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		lookups.Add(1)
		return nil, errors.New("DNS forbidden in preflight")
	}}
	defer func() { net.DefaultResolver = original }()
	for _, kind := range []string{"STT primary", "STT fallback", "cleanup active"} {
		cfg := addressValidationConfig(kind, "provider.example.invalid", "lan")
		if err := Validate(cfg); err != nil {
			t.Fatalf("hostname rejected for %s: %v", kind, err)
		}
		cfg = addressValidationConfig(kind, "localhost", "local")
		if err := Validate(cfg); err != nil {
			t.Fatalf("localhost rejected for %s: %v", kind, err)
		}
	}
	if lookups.Load() != 0 {
		t.Fatalf("preflight attempted %d DNS lookups", lookups.Load())
	}
}

func addressValidationConfig(kind, host, zone string) Config {
	cfg := Defaults()
	cfg.Policy.STTZones = []string{"local", "lan", "internet"}
	cfg.Policy.CleanupZones = []string{"local", "lan", "internet"}
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.BaseURL = "http://" + net.JoinHostPort(host, "8000") + "/v1"
	provider.Zone = zone
	switch kind {
	case "STT primary":
		cfg.STT.Providers[cfg.STT.Provider] = provider
	case "STT fallback":
		cfg.STT.Fallback = "fallback"
		cfg.STT.Providers["fallback"] = provider
	case "STT inactive":
		cfg.STT.Providers["inactive"] = provider
	case "cleanup active", "cleanup inactive":
		cfg.Cleanup.Providers["clean"] = CleanupProviderConfig{
			Type: provider.Type, BaseURL: provider.BaseURL, Model: "cleaner", TimeoutS: 10, Zone: zone,
		}
		if kind == "cleanup active" {
			cfg.Cleanup.Enabled = true
			cfg.Cleanup.Provider = "clean"
		}
	}
	return cfg
}
