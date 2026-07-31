package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

func TestExamplesResolve(t *testing.T) {
	for _, name := range []string{"config.minimal.toml", "config.full.toml"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Resolve(filepath.Join("..", "..", "examples", name), Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.STT.Provider == "" || len(cfg.STT.Providers) == 0 {
				t.Fatalf("incomplete resolved config: %#v", cfg.STT)
			}
		})
	}
}

func TestResolveRejectsUnknownTOMLKey(t *testing.T) {
	path := writeConfig(t, "[audio]\nkeep_recording = true\n")
	_, err := Resolve(path, Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "audio.keep_recording") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolveConsumesLoggingLevel(t *testing.T) {
	path := writeConfig(t, "[logging]\nlevel = \"debug\"\n")
	cfg, err := Resolve(path, Overrides{})
	if err != nil || cfg.Logging.Level != "debug" {
		t.Fatalf("Resolve() logging=%#v error=%v", cfg.Logging, err)
	}
}

func TestResolveRejectsPlaintextSecret(t *testing.T) {
	path := writeConfig(t, `[stt.providers.local]
api_key = "must-not-appear-in-errors"
`)
	_, err := Resolve(path, Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("class = %q, error = %v", errs.Classify(err), err)
	}
	if strings.Contains(err.Error(), "must-not-appear") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

func TestCleanupProviderRejectsSTTOnlyFields(t *testing.T) {
	for _, field := range []string{"language", "prompt", "health_endpoint", "health_enabled", "retries", "retry_backoff_ms"} {
		t.Run(field, func(t *testing.T) {
			path := writeConfig(t, "[cleanup.providers.clean]\n"+field+" = "+map[string]string{
				"language": `"fr"`, "prompt": `"instruction"`, "health_endpoint": `"/health"`,
				"health_enabled": "true", "retries": "1", "retry_backoff_ms": "100",
			}[field]+"\n")
			_, err := Resolve(path, Overrides{})
			if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "cleanup.providers.clean."+field) {
				t.Fatalf("field %s error=%v", field, err)
			}
		})
	}
}

func TestResolvePrecedence(t *testing.T) {
	path := writeConfig(t, "[sink]\nmode = \"stdout\"\n")
	flagValue := "clipboard"
	cfg, err := ResolveWithOptions(ResolveOptions{
		Path: path,
		LookupEnv: func(name string) (string, bool) {
			if name == "MURMUR_SINK_MODE" {
				return "type", true
			}
			return "", false
		},
		Overrides: Overrides{SinkMode: &flagValue},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sink.Mode != "clipboard" {
		t.Fatalf("sink mode = %q, want flag value", cfg.Sink.Mode)
	}

	cfg, err = ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: func(name string) (string, bool) {
		return "", false
	}})
	if err != nil || cfg.Sink.Mode != "stdout" {
		t.Fatalf("file did not override defaults: mode=%q error=%v", cfg.Sink.Mode, err)
	}
}

func TestResolveAppliesExplicitAutoMode(t *testing.T) {
	path := writeConfig(t, "[general]\nprofile = \"lan\"\n[sink]\nmode = \"auto\"\nauto_mode = \"type\"\n")
	cfg, err := ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sink.Mode != "type" {
		t.Fatalf("effective sink = %q", cfg.Sink.Mode)
	}
}

func TestResolveRejectsInvalidEnvironment(t *testing.T) {
	_, err := ResolveWithOptions(ResolveOptions{LookupEnv: func(name string) (string, bool) {
		if name == "MURMUR_STT_CHUNK_SECONDS" {
			return "soon", true
		}
		return "", false
	}})
	if errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("error = %v", err)
	}
}

func TestValidationRules(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"negative chunk", func(c *Config) { c.STT.ChunkSeconds = -1 }, "chunk_seconds"},
		{"live without chunk", func(c *Config) { c.STT.LiveChunking = true }, "live_chunking"},
		{"zero workers", func(c *Config) { c.STT.ChunkWorkers = 0 }, "chunk_workers"},
		{"unknown profile", func(c *Config) { c.General.Profile = "desktop" }, "general.profile"},
		{"auto language missing", func(c *Config) {
			p := c.STT.Providers[c.STT.Provider]
			p.Language = ""
			c.STT.Providers[c.STT.Provider] = p
		}, "langue fr ou en explicite"},
		{"auto fallback language mismatch", func(c *Config) {
			fallback := c.STT.Providers[c.STT.Provider]
			fallback.Language = "en"
			c.STT.Providers["fallback"] = fallback
			c.STT.Fallback = "fallback"
		}, "même langue"},
		{"unknown primary", func(c *Config) { c.STT.Provider = "missing" }, "stt.provider"},
		{"unknown fallback", func(c *Config) { c.STT.Fallback = "missing" }, "stt.fallback"},
		{"unknown cleanup", func(c *Config) { c.Cleanup.Enabled = true; c.Cleanup.Provider = "missing" }, "cleanup.provider"},
		{"file path required", func(c *Config) { c.Sink.Mode = "file" }, "sink.file"},
		{"internet requires TLS", func(c *Config) {
			p := c.STT.Providers["local"]
			p.Zone = "internet"
			p.BaseURL = "http://203.0.113.1/v1"
			c.STT.Providers["local"] = p
			c.Policy.STTZones = []string{"internet"}
		}, "HTTPS"},
		{"local URL must be local", func(c *Config) {
			p := c.STT.Providers["local"]
			p.BaseURL = "https://example.com/v1"
			c.STT.Providers["local"] = p
		}, "n'est pas locale"},
		{"multiple secrets", func(c *Config) {
			p := c.STT.Providers["local"]
			p.APIKeyEnv = "TOKEN"
			p.APIKeyFile = "/secret"
			c.STT.Providers["local"] = p
		}, "qu'une source"},
		{"base URL userinfo", func(c *Config) {
			p := c.STT.Providers["local"]
			p.BaseURL = "http://user:secret@127.0.0.1/v1"
			c.STT.Providers["local"] = p
		}, "base_url"},
		{"base URL query", func(c *Config) {
			p := c.STT.Providers["local"]
			p.BaseURL = "http://127.0.0.1/v1?target=elsewhere"
			c.STT.Providers["local"] = p
		}, "base_url"},
		{"base URL fragment", func(c *Config) {
			p := c.STT.Providers["local"]
			p.BaseURL = "http://127.0.0.1/v1#fragment"
			c.STT.Providers["local"] = p
		}, "base_url"},
		{"health endpoint required", func(c *Config) {
			p := c.STT.Providers["local"]
			p.HealthEnabled = true
			c.STT.Providers["local"] = p
		}, "health_endpoint"},
		{"retries bounded", func(c *Config) {
			p := c.STT.Providers["local"]
			p.Retries = 11
			c.STT.Providers["local"] = p
		}, "retries"},
		{"invalid auto mode", func(c *Config) { c.Sink.AutoMode = "profile" }, "sink.auto_mode"},
		{"invalid OSD position", func(c *Config) { c.UI.OSD.Position = "bottom" }, "ui.osd.position"},
		{"invalid log level", func(c *Config) { c.Logging.Level = "trace" }, "logging.level"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			test.edit(&cfg)
			err := Validate(cfg)
			if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestResolveProfileDefaultsAndExplicitPrecedence(t *testing.T) {
	path := writeConfig(t, `[general]
profile = "lan"
[policy]
stt_zones = ["local"]
retain_diagnostics = true
[audio]
keep_recordings = true
[sink]
clipboard_mode = "none"
`)
	cfg, err := ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Policy.STTZones, ",") != "local" || strings.Join(cfg.Policy.CleanupZones, ",") != "local,lan" {
		t.Fatalf("profile zones = stt:%v cleanup:%v", cfg.Policy.STTZones, cfg.Policy.CleanupZones)
	}
	if !cfg.Policy.RetainDiagnostics || !cfg.Audio.KeepRecordings || cfg.Sink.ClipboardMode != "none" {
		t.Fatalf("explicit retention overwritten: %#v %#v %#v", cfg.Policy, cfg.Audio, cfg.Sink)
	}

	overrideZones := []string{"local"}
	profile := "cloud-no-history"
	cfg, err = ResolveWithOptions(ResolveOptions{
		Path: path,
		LookupEnv: func(name string) (string, bool) {
			if name == "MURMUR_PROFILE" {
				return "cloud", true
			}
			return "", false
		},
		Overrides: Overrides{Profile: &profile, CleanupZones: &overrideZones},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.General.Profile != "cloud-no-history" || strings.Join(cfg.Policy.CleanupZones, ",") != "local" {
		t.Fatalf("override precedence failed: profile=%q cleanup=%v", cfg.General.Profile, cfg.Policy.CleanupZones)
	}
	if cfg.Sink.ClipboardMode != "none" || !cfg.Policy.RetainDiagnostics || !cfg.Audio.KeepRecordings {
		t.Fatalf("explicit lower-layer values were overwritten: %#v", cfg)
	}
}

func TestDataProfileMatrix(t *testing.T) {
	tests := []struct {
		profile string
		stt     string
		cleanup string
		clip    string
	}{
		{"offline", "local", "local", "clean-only"},
		{"local", "local", "local", "clean-only"},
		{"lan", "local,lan", "local,lan", "clean-only"},
		{"lan-cloud-cleanup", "local,lan", "local,lan,internet", "clean-only"},
		{"cloud", "local,lan,internet", "local,lan,internet", "clean-only"},
		{"cloud-no-history", "local,lan,internet", "local,lan,internet", "none"},
	}
	for _, test := range tests {
		t.Run(test.profile, func(t *testing.T) {
			path := writeConfig(t, "[general]\nprofile = \""+test.profile+"\"\n")
			cfg, err := ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(cfg.Policy.STTZones, ",") != test.stt || strings.Join(cfg.Policy.CleanupZones, ",") != test.cleanup || cfg.Sink.ClipboardMode != test.clip {
				t.Fatalf("profile %s = stt:%v cleanup:%v clipboard:%s", test.profile, cfg.Policy.STTZones, cfg.Policy.CleanupZones, cfg.Sink.ClipboardMode)
			}
		})
	}
}

func TestProviderPolicyViolationIsPolicyBlocked(t *testing.T) {
	cfg := Defaults()
	cfg.Policy.STTZones = []string{"lan"}
	err := Validate(cfg)
	if errs.Classify(err) != errs.PolicyBlocked || !strings.Contains(err.Error(), "interdite") {
		t.Fatalf("Validate() error=%v class=%s", err, errs.Classify(err))
	}
}

func TestValidateClipboardOnly(t *testing.T) {
	for _, mode := range []string{"none", "transient"} {
		cfg := Defaults()
		cfg.Sink.ClipboardMode = mode
		if err := ValidateClipboardOnly(cfg, true); errs.Classify(err) != errs.ConfigInvalid {
			t.Errorf("mode %q accepted", mode)
		}
	}
	for _, mode := range []string{"raw-and-clean", "clean-only"} {
		cfg := Defaults()
		cfg.Sink.ClipboardMode = mode
		if err := ValidateClipboardOnly(cfg, true); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
}

type fakeRunner struct {
	output []byte
	err    error
	name   string
	args   []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string) ([]byte, error) {
	f.name = name
	f.args = append([]string(nil), args...)
	return f.output, f.err
}

func TestSecretResolverSources(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		resolver := SecretResolver{LookupEnv: func(name string) (string, bool) {
			return " env-secret \n", name == "TOKEN"
		}}
		got, err := resolver.Resolve(context.Background(), ProviderConfig{APIKeyEnv: "TOKEN"})
		if err != nil || got != "env-secret" {
			t.Fatalf("Resolve() = %q, %v", got, err)
		}
	})

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: path})
		if err != nil || got != "file-secret" {
			t.Fatalf("Resolve() = %q, %v", got, err)
		}
	})

	t.Run("command", func(t *testing.T) {
		runner := &fakeRunner{output: []byte("command-secret\n")}
		got, err := (SecretResolver{Runner: runner}).Resolve(context.Background(), ProviderConfig{APIKeyCommand: []string{"secret-tool", "lookup", "murmur"}})
		if err != nil || got != "command-secret" {
			t.Fatalf("Resolve() = %q, %v", got, err)
		}
		if runner.name != "secret-tool" || strings.Join(runner.args, " ") != "lookup murmur" {
			t.Fatalf("runner called with %q %#v", runner.name, runner.args)
		}
	})
}

func TestSecretResolverRejectsLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("do-not-leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: path})
	if errs.Classify(err) != errs.ConfigInvalid || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretResolverRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token")
	if err := os.WriteFile(target, []byte("do-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: link})
	if errs.Classify(err) != errs.ConfigInvalid || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretResolverRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, MaxSecretBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: path})
	if errs.Classify(err) != errs.AuthFailed || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretResolverRejectsOversizedHelperOutput(t *testing.T) {
	runner := &fakeRunner{output: bytes.Repeat([]byte{'x'}, MaxSecretBytes+1)}
	_, err := (SecretResolver{Runner: runner}).Resolve(context.Background(), ProviderConfig{APIKeyCommand: []string{"helper"}})
	if errs.Classify(err) != errs.AuthFailed || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretResolverDoesNotExposeFailedCommandOutput(t *testing.T) {
	runner := &fakeRunner{output: []byte("do-not-leak"), err: errors.New("helper stderr")}
	_, err := (SecretResolver{Runner: runner}).Resolve(context.Background(), ProviderConfig{APIKeyCommand: []string{"helper"}})
	if errs.Classify(err) != errs.AuthFailed || strings.Contains(err.Error(), "do-not-leak") || strings.Contains(err.Error(), "helper stderr") {
		t.Fatalf("error = %v", err)
	}
}

type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ []string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSecretCommandIsBoundedAndCancellable(t *testing.T) {
	provider := ProviderConfig{APIKeyCommand: []string{"blocked"}}
	resolver := SecretResolver{Runner: blockingRunner{}, CommandTimeout: 20 * time.Millisecond}
	started := time.Now()
	_, err := resolver.Resolve(context.Background(), provider)
	if errs.Classify(err) != errs.Timeout || time.Since(started) > time.Second {
		t.Fatalf("timeout error=%v duration=%v", err, time.Since(started))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (SecretResolver{Runner: blockingRunner{}}).Resolve(ctx, provider)
	if errs.Classify(err) != errs.Cancelled {
		t.Fatalf("cancel error=%v", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
