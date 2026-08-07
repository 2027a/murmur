package config

import (
	"os"
	"strings"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

func TestPluginConfigurationDefaultsAndValidation(t *testing.T) {
	defaults := Defaults()
	if defaults.Plugin.Enabled || defaults.Plugin.Mode != "plain" || defaults.Plugin.TimeoutS <= 0 {
		t.Fatalf("plugin defaults = %#v", defaults.Plugin)
	}

	tests := []struct {
		name  string
		edit  func(*Config)
		class errs.Class
		want  string
	}{
		{"unknown mode", func(cfg *Config) { cfg.Plugin.Mode = "html" }, errs.ConfigInvalid, "plugin.mode"},
		{"zero timeout", func(cfg *Config) { cfg.Plugin.TimeoutS = 0 }, errs.ConfigInvalid, "plugin.timeout_s"},
		{"forbidden zone", func(cfg *Config) {
			cfg.Plugin.Enabled = true
			cfg.Plugin.Mode = "markdown"
			cfg.Policy.PluginZones = []string{"lan"}
		}, errs.PolicyBlocked, "policy.plugin_zones"},
		{"unknown zone", func(cfg *Config) { cfg.Policy.PluginZones = []string{"remote"} }, errs.ConfigInvalid, "zone invalide"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			test.edit(&cfg)
			err := Validate(cfg)
			if errs.Classify(err) != test.class || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPluginConfigurationResolvesFromEnvironment(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", base)
	cfg, err := ResolveWithOptions(ResolveOptions{LookupEnv: func(name string) (string, bool) {
		values := map[string]string{
			"MURMUR_PLUGIN_ENABLED":   "true",
			"MURMUR_PLUGIN_MODE":      "markdown",
			"MURMUR_PLUGIN_TIMEOUT_S": "3",
		}
		value, ok := values[name]
		return value, ok
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Plugin.Enabled || cfg.Plugin.Mode != "markdown" || cfg.Plugin.TimeoutS != 3 {
		t.Fatalf("resolved plugin config = %#v", cfg.Plugin)
	}
}
