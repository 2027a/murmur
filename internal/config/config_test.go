package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
)

func TestExamplesResolve(t *testing.T) {
	for _, name := range []string{"config.minimal.toml", "config.full.toml"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Resolve(writeConfig(t, string(body)), Overrides{})
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

func TestCleanupReasoningEffortTOML(t *testing.T) {
	for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "automatic", "LOW"} {
		t.Run("effort="+effort, func(t *testing.T) {
			path := writeConfig(t, `[cleanup.providers.test]
type = "openai-compatible"
base_url = "http://127.0.0.1:8001/v1"
model = "test"
timeout_s = 10
zone = "local"
reasoning_effort = "`+effort+`"
`)
			cfg, err := ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
			if effort == "automatic" || effort == "LOW" {
				if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "cleanup.providers.test.reasoning_effort") {
					t.Fatalf("invalid effort accepted: %v", err)
				}
				return
			}
			if err != nil || cfg.Cleanup.Providers["test"].ReasoningEffort != effort {
				t.Fatalf("effort not loaded: %v", err)
			}
		})
	}
}

func TestShortLiveDefaultsAndExplicitBatchOptOut(t *testing.T) {
	for _, test := range []struct {
		name, body string
		live       bool
		seconds    int
		invalid    bool
	}{
		{"defaults", "", true, 5, false},
		{"old explicit opt-out", "[stt]\nchunk_seconds=0\n", false, 0, false},
		{"explicit batch", "[stt]\nlive_chunking=false\n", false, 5, false},
		{"explicit duration", "[stt]\nchunk_seconds=60\n", true, 60, false},
		{"conflicting explicit keys", "[stt]\nchunk_seconds=0\nlive_chunking=true\n", false, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := ResolveWithOptions(ResolveOptions{Path: writeConfig(t, test.body), LookupEnv: func(string) (string, bool) { return "", false }})
			if test.invalid {
				if errs.Classify(err) != errs.ConfigInvalid {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil || cfg.STT.LiveChunking != test.live || cfg.STT.ChunkSeconds != test.seconds || cfg.STT.ChunkPauseMS != 500 || cfg.STT.ChunkOverlapMS != 0 {
				t.Fatalf("stt=%#v err=%v", cfg.STT, err)
			}
		})
	}
	_, err := ResolveWithOptions(ResolveOptions{Path: writeConfig(t, "[stt]\nchunk_seconds=0\n"), LookupEnv: func(name string) (string, bool) { return "true", name == "MURMUR_STT_LIVE_CHUNKING" }})
	if errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("explicit env live must fail: %v", err)
	}
}

func TestResolveConsumesOperationalLoggingConfig(t *testing.T) {
	path := writeConfig(t, "[logging]\nlevel = \"debug\"\nmax_bytes = 2048\nmax_files = 2\n")
	cfg, err := Resolve(path, Overrides{})
	if err != nil || cfg.Logging.Level != "debug" || cfg.Logging.MaxBytes != 2048 || cfg.Logging.MaxFiles != 2 {
		t.Fatalf("Resolve() logging=%#v error=%v", cfg.Logging, err)
	}
	cfg, err = ResolveWithOptions(ResolveOptions{Path: writeConfig(t, ""), LookupEnv: func(name string) (string, bool) {
		switch name {
		case "MURMUR_LOG_MAX_BYTES":
			return "4096", true
		case "MURMUR_LOG_MAX_FILES":
			return "4", true
		default:
			return "", false
		}
	}})
	if err != nil || cfg.Logging.MaxBytes != 4096 || cfg.Logging.MaxFiles != 4 {
		t.Fatalf("environment logging=%#v error=%v", cfg.Logging, err)
	}
}

func TestSTTResourceLimitsResolveFromTOMLAndEnvironment(t *testing.T) {
	defaults := Defaults().STT
	if defaults.MaxTextBytes != 16<<20 || defaults.MaxSegments != 4096 {
		t.Fatalf("STT defaults=%#v", defaults)
	}
	path := writeConfig(t, "[stt]\nmax_text_bytes = 12345\nmax_segments = 123\n")
	cfg, err := Resolve(path, Overrides{})
	if err != nil || cfg.STT.MaxTextBytes != 12345 || cfg.STT.MaxSegments != 123 {
		t.Fatalf("STT config=%#v error=%v", cfg.STT, err)
	}
	cfg, err = ResolveWithOptions(ResolveOptions{Path: writeConfig(t, ""), LookupEnv: func(name string) (string, bool) {
		switch name {
		case "MURMUR_STT_MAX_TEXT_BYTES":
			return "23456", true
		case "MURMUR_STT_MAX_SEGMENTS":
			return "234", true
		default:
			return "", false
		}
	}})
	if err != nil || cfg.STT.MaxTextBytes != 23456 || cfg.STT.MaxSegments != 234 {
		t.Fatalf("STT environment config=%#v error=%v", cfg.STT, err)
	}
}

func TestTypeNewlinesDefaultAndPreflight(t *testing.T) {
	if got := Defaults().Sink.TypeNewlines; got != "space" {
		t.Fatalf("default type_newlines = %q, want space", got)
	}
	for _, mode := range []string{"space", "marker", "dangerous-keep"} {
		t.Run(mode, func(t *testing.T) {
			path := writeConfig(t, "[sink]\ntype_newlines = \""+mode+"\"\n")
			cfg, err := Resolve(path, Overrides{})
			if err != nil || cfg.Sink.TypeNewlines != mode {
				t.Fatalf("Resolve() type_newlines=%q error=%v", cfg.Sink.TypeNewlines, err)
			}
		})
	}
	t.Run("legacy keep rejected", func(t *testing.T) {
		_, err := Resolve(writeConfig(t, "[sink]\ntype_newlines = \"keep\"\n"), Overrides{})
		if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "type_newlines") {
			t.Fatalf("Resolve() error=%v", err)
		}
	})
}

func TestResolveConfigFilePermissions(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
		ok   bool
	}{
		{name: "0600", mode: 0o600, ok: true},
		{name: "0644", mode: 0o644, ok: true},
		{name: "group writable", mode: 0o620},
		{name: "world writable", mode: 0o602},
		{name: "group and world writable", mode: 0o666},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, "[logging]\nlevel = \"debug\"\n")
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			cfg, err := Resolve(path, Overrides{})
			if test.ok {
				if err != nil || cfg.Logging.Level != "debug" {
					t.Fatalf("Resolve() config=%#v error=%v", cfg.Logging, err)
				}
				return
			}
			if errs.Classify(err) != errs.ConfigInvalid || !errorChainContains(err, "modifiable") {
				t.Fatalf("Resolve() error=%v", err)
			}
		})
	}
}

func TestResolveRejectsConfigOwnedByAnotherUser(t *testing.T) {
	original := configFstat
	configFstat = func(fd int) (syscall.Stat_t, error) {
		stat, err := original(fd)
		stat.Uid = uint32(os.Geteuid()) + 1
		return stat, err
	}
	defer func() { configFstat = original }()

	_, err := Resolve(writeConfig(t, ""), Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid || !errorChainContains(err, "n'appartient pas") {
		t.Fatalf("Resolve() error=%v", err)
	}
}

func TestResolveAcceptsConfigSymlinksAtEveryLevel(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real", "nested")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realParent, "config.toml")
	if err := os.WriteFile(target, []byte("[logging]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	finalLink := filepath.Join(realParent, "config-link.toml")
	if err := os.Symlink(target, finalLink); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(base, "parent-link")
	if err := os.Symlink(realParent, parentLink); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(base, "ancestor-link")
	if err := os.Symlink(filepath.Join(base, "real"), ancestorLink); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "final", path: finalLink},
		{name: "parent", path: filepath.Join(parentLink, "config.toml")},
		{name: "ancestor", path: filepath.Join(ancestorLink, "nested", "config.toml")},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Resolve(test.path, Overrides{})
			if err != nil || cfg.Logging.Level != "debug" {
				t.Fatalf("Resolve(%q) level=%q error=%v", test.path, cfg.Logging.Level, err)
			}
		})
	}
}

func TestResolveAcceptsImplicitXDGConfigDirectorySymlink(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "dotfiles")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "murmur")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", base)

	cfg, err := Resolve("", Overrides{})
	if err != nil || cfg.STT.Provider != "local" {
		t.Fatalf("Resolve() provider=%q error=%v", cfg.STT.Provider, err)
	}
}

func TestResolveRejectsConfigSymlinkRetargetedDuringLoad(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.toml")
	second := filepath.Join(dir, "second.toml")
	link := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(first, []byte("[logging]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("[logging]\nlevel = \"warn\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}

	original := configFstat
	var calls atomic.Int32
	configFstat = func(fd int) (syscall.Stat_t, error) {
		stat, err := original(fd)
		if calls.Add(1) == 1 {
			if removeErr := os.Remove(link); removeErr != nil {
				return stat, removeErr
			}
			if linkErr := os.Symlink(second, link); linkErr != nil {
				return stat, linkErr
			}
		}
		return stat, err
	}
	defer func() { configFstat = original }()

	_, err := Resolve(link, Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid || !errorChainContains(err, "remplacé") {
		t.Fatalf("Resolve() error=%v", err)
	}
}

func TestResolveRejectsNonRegularConfigWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "directory", setup: func(path string) error { return os.Mkdir(path, 0o700) }},
		{name: "fifo", setup: func(path string) error { return syscall.Mkfifo(path, 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, test.name)
			if err := test.setup(path); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := Resolve(path, Overrides{})
				done <- err
			}()
			select {
			case err := <-done:
				if errs.Classify(err) != errs.ConfigInvalid {
					t.Fatalf("Resolve() error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Resolve() bloqué sur un fichier non régulier")
			}
		})
	}
}

func TestResolveRejectsConfigReplacedDuringLoad(t *testing.T) {
	path := writeConfig(t, "[logging]\nlevel = \"debug\"\n")
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte("[logging]\nlevel = \"warn\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	original := configFstat
	var calls atomic.Int32
	replace := make(chan struct{})
	replaced := make(chan error, 1)
	go func() {
		<-replace
		replaced <- os.Rename(replacement, path)
	}()
	configFstat = func(fd int) (syscall.Stat_t, error) {
		stat, err := original(fd)
		if calls.Add(1) == 1 {
			close(replace)
			if renameErr := <-replaced; renameErr != nil {
				return stat, renameErr
			}
		}
		return stat, err
	}
	defer func() { configFstat = original }()

	_, err := Resolve(path, Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid || !errorChainContains(err, "remplacé") {
		t.Fatalf("Resolve() error=%v", err)
	}
}

func TestResolveRejectsUnstableConfigInode(t *testing.T) {
	original := configFstat
	var calls atomic.Int32
	configFstat = func(fd int) (syscall.Stat_t, error) {
		stat, err := original(fd)
		if calls.Add(1) == 2 {
			stat.Ino++
		}
		return stat, err
	}
	defer func() { configFstat = original }()

	_, err := Resolve(writeConfig(t, ""), Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid || !errorChainContains(err, "inode") {
		t.Fatalf("Resolve() error=%v", err)
	}
}

func TestResolveRejectsConfigRewrittenInPlaceDuringRead(t *testing.T) {
	path := writeConfig(t, "[logging]\nlevel = \"debug\"\n")
	replacement := []byte("[logging]\nlevel = \"warn\" \n")
	original := configFstat
	var calls atomic.Int32
	configFstat = func(fd int) (syscall.Stat_t, error) {
		stat, err := original(fd)
		if calls.Add(1) == 1 {
			handle, openErr := os.OpenFile(path, os.O_WRONLY, 0)
			if openErr != nil {
				return stat, openErr
			}
			_, writeErr := handle.WriteAt(replacement, 0)
			closeErr := handle.Close()
			future := time.Now().Add(time.Hour)
			timeErr := os.Chtimes(path, future, future)
			if joined := errors.Join(writeErr, closeErr, timeErr); joined != nil {
				return stat, joined
			}
		}
		return stat, err
	}
	defer func() { configFstat = original }()

	_, err := Resolve(path, Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid || !errorChainContains(err, "modifié") {
		t.Fatalf("Resolve() error=%v", err)
	}
}

func TestResolveRejectsOversizedConfigSnapshot(t *testing.T) {
	path := writeConfig(t, string(bytes.Repeat([]byte{' '}, MaxConfigBytes+1)))
	_, err := Resolve(path, Overrides{})
	if errs.Classify(err) != errs.ConfigInvalid || !errorChainContains(err, "1 Mio") {
		t.Fatalf("Resolve() error=%v", err)
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
		{"live without chunk", func(c *Config) { c.STT.LiveChunking = true; c.STT.ChunkSeconds = 0 }, "live_chunking"},
		{"negative pause", func(c *Config) { c.STT.ChunkPauseMS = -1 }, "chunk_pause_ms"},
		{"oversized pause", func(c *Config) { c.STT.ChunkPauseMS = 1001 }, "chunk_pause_ms"},
		{"pause fills segment", func(c *Config) { c.STT.ChunkSeconds = 1; c.STT.ChunkPauseMS = 1000 }, "chunk_pause_ms"},
		{"zero workers", func(c *Config) { c.STT.ChunkWorkers = 0 }, "chunk_workers"},
		{"zero text budget", func(c *Config) { c.STT.MaxTextBytes = 0 }, "max_text_bytes"},
		{"excessive text budget", func(c *Config) { c.STT.MaxTextBytes = 64<<20 + 1 }, "max_text_bytes"},
		{"zero segment limit", func(c *Config) { c.STT.MaxSegments = 0 }, "max_segments"},
		{"excessive segment limit", func(c *Config) { c.STT.MaxSegments = 65537 }, "max_segments"},
		{"empty primary model", func(c *Config) {
			p := c.STT.Providers[c.STT.Provider]
			p.Model = " \t"
			c.STT.Providers[c.STT.Provider] = p
		}, "model"},
		{"empty cleanup model", func(c *Config) {
			c.Cleanup.Enabled = true
			c.Cleanup.Provider = "clean"
			c.Cleanup.Providers["clean"] = CleanupProviderConfig{
				Type: "openai-compatible", BaseURL: "http://127.0.0.1:8001/v1", Model: " ", TimeoutS: 30, Zone: "local",
			}
		}, "cleanup.providers.clean.model"},
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
		{"relative credential file", func(c *Config) {
			p := c.STT.Providers["local"]
			p.APIKeyFile = "openai.key"
			c.STT.Providers["local"] = p
		}, "api_key_file"},
		{"empty credential helper", func(c *Config) {
			p := c.STT.Providers["local"]
			p.APIKeyCommand = []string{""}
			c.STT.Providers["local"] = p
		}, "api_key_command"},
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
		{"internet health requires TLS", func(c *Config) {
			p := c.STT.Providers["local"]
			p.BaseURL = "https://example.com/v1"
			p.Zone = "internet"
			p.HealthEnabled = true
			p.HealthEndpoint = "http://example.com/health"
			c.STT.Providers["local"] = p
			c.Policy.STTZones = []string{"internet"}
		}, "health_endpoint doit utiliser HTTPS"},
		{"cross-origin health", func(c *Config) {
			p := c.STT.Providers["local"]
			p.BaseURL = "http://example.com/v1"
			p.Zone = "internet"
			p.AllowInsecure = true
			p.HealthEnabled = true
			p.HealthEndpoint = "http://other.example.com/health"
			c.STT.Providers["local"] = p
			c.Policy.STTZones = []string{"internet"}
		}, "même origine"},
		{"network-path health", func(c *Config) {
			p := c.STT.Providers["local"]
			p.HealthEndpoint = "//localhost:9000/health"
			c.STT.Providers["local"] = p
		}, "même origine"},
		{"disabled invalid health", func(c *Config) {
			p := c.STT.Providers["local"]
			p.HealthEndpoint = "mailto:health@example.com"
			c.STT.Providers["local"] = p
		}, "health_endpoint"},
		{"retries bounded", func(c *Config) {
			p := c.STT.Providers["local"]
			p.Retries = 11
			c.STT.Providers["local"] = p
		}, "retries"},
		{"unknown paste shortcut", func(c *Config) { c.Sink.PasteShortcuts = []string{"ctrl+v", "super+v"} }, "paste_shortcuts[1]"},
		{"clipboard sink without persistence", func(c *Config) {
			c.Sink.Mode = "clipboard"
			c.Sink.ClipboardMode = "transient"
		}, "sink.mode=clipboard"},
		{"invalid auto mode", func(c *Config) { c.Sink.AutoMode = "profile" }, "sink.auto_mode"},
		{"invalid newline mode", func(c *Config) { c.Sink.TypeNewlines = "keep" }, "sink.type_newlines"},
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

func TestTransientClipboardPreflightUsesEffectiveInjectionMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		auto    string
		clip    string
		shorts  []string
		wantErr bool
	}{
		{name: "paste shortcut", mode: "paste", auto: "type", clip: "transient", shorts: []string{"ctrl+v"}, wantErr: true},
		{name: "auto paste shortcut", mode: "auto", auto: "paste", clip: "transient", shorts: []string{"ctrl+v"}, wantErr: true},
		{name: "paste direct fallback", mode: "paste", auto: "type", clip: "transient", shorts: []string{}},
		{name: "auto type", mode: "auto", auto: "type", clip: "transient", shorts: []string{"ctrl+v"}},
		{name: "type", mode: "type", auto: "paste", clip: "transient", shorts: []string{"ctrl+v"}},
		{name: "paste without clipboard", mode: "paste", auto: "type", clip: "none", shorts: []string{"ctrl+v"}},
		{name: "stdout", mode: "stdout", auto: "paste", clip: "transient", shorts: []string{"ctrl+v"}},
		{name: "file", mode: "file", auto: "paste", clip: "transient", shorts: []string{"ctrl+v"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Sink.Mode = test.mode
			cfg.Sink.AutoMode = test.auto
			cfg.Sink.ClipboardMode = test.clip
			cfg.Sink.PasteShortcuts = test.shorts
			if test.mode == "file" {
				cfg.Sink.File = "result.txt"
			}
			err := Validate(cfg)
			if test.wantErr {
				if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "ne confirme pas la consommation") {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestNumericValidationBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		minimum  int
		maximum  int
		field    string
		setValue func(*Config, int)
	}{
		{"chunk seconds", 0, MaximumDurationSeconds, "chunk_seconds", func(c *Config, value int) { c.STT.ChunkSeconds = value; c.STT.LiveChunking = false }},
		{"chunk workers", 1, MaximumChunkWorkers, "chunk_workers", func(c *Config, value int) { c.STT.ChunkWorkers = value }},
		{"text bytes", 1, MaximumTextBytes, "max_text_bytes", func(c *Config, value int) { c.STT.MaxTextBytes = value }},
		{"segments", 1, MaximumSegments, "max_segments", func(c *Config, value int) { c.STT.MaxSegments = value }},
		{"STT timeout", 1, MaximumDurationSeconds, "timeout_s", func(c *Config, value int) {
			provider := c.STT.Providers[c.STT.Provider]
			provider.TimeoutS = value
			c.STT.Providers[c.STT.Provider] = provider
		}},
		{"cleanup timeout", 1, MaximumDurationSeconds, "timeout_s", func(c *Config, value int) {
			c.Cleanup.Enabled = true
			c.Cleanup.Provider = "clean"
			c.Cleanup.Providers["clean"] = CleanupProviderConfig{
				Type: "openai-compatible", BaseURL: "http://127.0.0.1:8001/v1", Model: "cleaner", TimeoutS: value, Zone: "local",
			}
		}},
		{"plugin timeout", 1, MaximumDurationSeconds, "plugin.timeout_s", func(c *Config, value int) { c.Plugin.TimeoutS = value }},
		{"provider retries", 0, MaximumProviderRetries, "retries", func(c *Config, value int) {
			provider := c.STT.Providers[c.STT.Provider]
			provider.Retries = value
			c.STT.Providers[c.STT.Provider] = provider
		}},
		{"retry backoff", 0, MaximumRetryBackoffMilliseconds, "retry_backoff_ms", func(c *Config, value int) {
			provider := c.STT.Providers[c.STT.Provider]
			provider.RetryBackoffMS = value
			c.STT.Providers[c.STT.Provider] = provider
		}},
		{"OSD success timeout", 0, MaximumDurationMilliseconds, "ui.osd", func(c *Config, value int) { c.UI.OSD.SuccessTimeoutMS = value }},
		{"OSD error timeout", 0, MaximumDurationMilliseconds, "ui.osd", func(c *Config, value int) { c.UI.OSD.ErrorTimeoutMS = value }},
		{"log bytes", 1024, MaximumLogBytes, "max_bytes", func(c *Config, value int) { c.Logging.MaxBytes = value }},
		{"log files", 1, MaximumLogFiles, "max_files", func(c *Config, value int) { c.Logging.MaxFiles = value }},
		{"journal bytes", 1024, MaximumSessionJournalBytes, "session_max_bytes", func(c *Config, value int) { c.Logging.SessionMaxBytes = value }},
		{"journal files", 1, MaximumSessionJournalFiles, "session_max_files", func(c *Config, value int) { c.Logging.SessionMaxFiles = value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, boundary := range []struct {
				name  string
				value int
				valid bool
			}{
				{"below", test.minimum - 1, false},
				{"minimum", test.minimum, true},
				{"maximum", test.maximum, true},
				{"above", test.maximum + 1, false},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					cfg := Defaults()
					test.setValue(&cfg, boundary.value)
					err := Validate(cfg)
					if boundary.valid && err != nil {
						t.Fatalf("Validate() error = %v", err)
					}
					if !boundary.valid && (errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), test.field)) {
						t.Fatalf("Validate() error = %v, want config_invalid containing %q", err, test.field)
					}
				})
			}
		})
	}
}

func TestHealthEndpointValidationAcceptsSameOriginBoundaries(t *testing.T) {
	tests := []struct {
		name          string
		baseURL       string
		health        string
		allowInsecure bool
	}{
		{name: "root relative", baseURL: "https://example.com/v1", health: "/health"},
		{name: "base relative", baseURL: "https://example.com/v1", health: "health?ready=1"},
		{name: "explicit default port", baseURL: "https://example.com/v1", health: "https://example.com:443/health"},
		{name: "insecure opt-in", baseURL: "http://example.com/v1", health: "http://example.com/health", allowInsecure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			provider := cfg.STT.Providers[cfg.STT.Provider]
			provider.BaseURL = test.baseURL
			provider.Zone = "internet"
			provider.AllowInsecure = test.allowInsecure
			provider.HealthEnabled = true
			provider.HealthEndpoint = test.health
			cfg.STT.Providers[cfg.STT.Provider] = provider
			cfg.Policy.STTZones = []string{"internet"}
			if err := Validate(cfg); err != nil {
				t.Fatalf("Validate() error = %v", err)
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

func TestValidateProvidersOnceInDeterministicOrder(t *testing.T) {
	t.Run("primary before fallback and inactive", func(t *testing.T) {
		testCfg := providerValidationConfig()
		provider := testCfg.STT.Providers[testCfg.STT.Provider]
		provider.Model = ""
		testCfg.STT.Providers[testCfg.STT.Provider] = provider
		provider = testCfg.STT.Providers["fallback"]
		provider.Model = ""
		testCfg.STT.Providers["fallback"] = provider
		provider = testCfg.STT.Providers["aaa-invalid"]
		provider.Model = ""
		testCfg.STT.Providers["aaa-invalid"] = provider

		err := Validate(testCfg)
		if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "stt.providers.local.model") {
			t.Fatalf("Validate() error = %v", err)
		}
	})

	t.Run("fallback before sorted inactive", func(t *testing.T) {
		testCfg := providerValidationConfig()
		provider := testCfg.STT.Providers["fallback"]
		provider.Model = ""
		testCfg.STT.Providers["fallback"] = provider
		provider = testCfg.STT.Providers["aaa-invalid"]
		provider.Model = ""
		testCfg.STT.Providers["aaa-invalid"] = provider

		err := Validate(testCfg)
		if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "stt.providers.fallback.model") {
			t.Fatalf("Validate() error = %v", err)
		}
	})

	t.Run("inactive sorted", func(t *testing.T) {
		testCfg := providerValidationConfig()
		provider := testCfg.STT.Providers["zzz-invalid"]
		provider.Model = ""
		testCfg.STT.Providers["zzz-invalid"] = provider
		provider = testCfg.STT.Providers["aaa-invalid"]
		provider.Model = ""
		testCfg.STT.Providers["aaa-invalid"] = provider

		err := Validate(testCfg)
		if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "stt.providers.aaa-invalid.model") {
			t.Fatalf("Validate() error = %v", err)
		}
	})

	t.Run("cleanup active before sorted inactive", func(t *testing.T) {
		testCfg := Defaults()
		provider := CleanupProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:8001/v1", Model: "cleaner", TimeoutS: 10, Zone: "local"}
		testCfg.Cleanup.Enabled = true
		testCfg.Cleanup.Provider = "cleanup"
		testCfg.Cleanup.Providers["cleanup"] = provider
		testCfg.Cleanup.Providers["aaa-invalid"] = provider
		testCfg.Cleanup.Providers["zzz-invalid"] = provider
		provider.Model = ""
		testCfg.Cleanup.Providers["cleanup"] = provider
		testCfg.Cleanup.Providers["aaa-invalid"] = provider

		err := Validate(testCfg)
		if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "cleanup.providers.cleanup.model") {
			t.Fatalf("Validate() error = %v", err)
		}
	})
}

func providerValidationConfig() Config {
	cfg := Defaults()
	primary := cfg.STT.Providers[cfg.STT.Provider]
	cfg.STT.Fallback = "fallback"
	cfg.STT.Providers["fallback"] = primary
	cfg.STT.Providers["aaa-invalid"] = primary
	cfg.STT.Providers["zzz-invalid"] = primary
	return cfg
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
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: path})
	if errs.Classify(err) != errs.ConfigInvalid || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretResolverRejectsSymlinksAtEveryLevel(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real", "nested")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realParent, "token")
	if err := os.WriteFile(target, []byte("do-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalLink := filepath.Join(realParent, "token-link")
	if err := os.Symlink(target, finalLink); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(base, "parent-link")
	if err := os.Symlink(realParent, parentLink); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(base, "ancestor-link")
	if err := os.Symlink(filepath.Join(base, "real"), ancestorLink); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "final", path: finalLink},
		{name: "parent", path: filepath.Join(parentLink, "token")},
		{name: "ancestor", path: filepath.Join(ancestorLink, "nested", "token")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: test.path})
			if errs.Classify(err) != errs.ConfigInvalid || strings.Contains(err.Error(), "do-not-leak") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSecretResolverRejectsRelativeFile(t *testing.T) {
	_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: "token"})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "absolu") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretResolverRejectsFileOwnedByAnotherUser(t *testing.T) {
	original := credentialFstat
	credentialFstat = func(fd int) (syscall.Stat_t, error) {
		stat, err := original(fd)
		stat.Uid = uint32(os.Geteuid()) + 1
		return stat, err
	}
	defer func() { credentialFstat = original }()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("do-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: path})
	if errs.Classify(err) != errs.ConfigInvalid || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretResolverRejectsNonRegularFileWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "directory", setup: func(path string) error { return os.Mkdir(path, 0o700) }},
		{name: "fifo", setup: func(path string) error { return syscall.Mkfifo(path, 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, test.name)
			if err := test.setup(path); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := (SecretResolver{}).Resolve(context.Background(), ProviderConfig{APIKeyFile: path})
				done <- err
			}()
			select {
			case err := <-done:
				if errs.Classify(err) != errs.ConfigInvalid {
					t.Fatalf("Resolve() error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Resolve() bloqué sur un fichier de credential non régulier")
			}
		})
	}
}

func TestSecretResolverParentReplacementCannotRedirectRead(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "credentials")
	hiddenParent := filepath.Join(base, "credentials.real")
	attackerParent := filepath.Join(base, "attacker")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(attackerParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "token"), []byte("expected-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attackerParent, "token"), []byte("redirected-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(parent, "token")
	resolver := SecretResolver{}
	if got, err := resolver.Resolve(context.Background(), ProviderConfig{APIKeyFile: path}); err != nil || got != "expected-secret" {
		t.Fatalf("initial Resolve() = %q, %v", got, err)
	}

	stop := make(chan struct{})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		first := true
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := os.Rename(parent, hiddenParent); err != nil {
				done <- err
				return
			}
			if err := os.Symlink(attackerParent, parent); err != nil {
				done <- err
				return
			}
			if first {
				close(started)
				first = false
			}
			if err := os.Remove(parent); err != nil {
				done <- err
				return
			}
			if err := os.Rename(hiddenParent, parent); err != nil {
				done <- err
				return
			}
		}
	}()

	<-started
	for range 1000 {
		got, err := resolver.Resolve(context.Background(), ProviderConfig{APIKeyFile: path})
		if err == nil && got != "expected-secret" {
			close(stop)
			<-done
			t.Fatalf("Resolve() followed a replacement parent: %q", got)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
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

func TestCredentialHelperEnvironmentRemovesAllProviderCredentialVariables(t *testing.T) {
	cfg := Defaults()
	primary := cfg.STT.Providers[cfg.STT.Provider]
	primary.APIKeyEnv = "PRIMARY_KEY"
	cfg.STT.Providers[cfg.STT.Provider] = primary
	cfg.STT.Providers["fallback"] = ProviderConfig{APIKeyEnv: "SHARED_KEY"}
	cfg.STT.Providers["inactive"] = ProviderConfig{APIKeyEnv: "inactive_key"}
	cfg.Cleanup.Providers["active"] = CleanupProviderConfig{APIKeyEnv: "SHARED_KEY"}
	cfg.Cleanup.Providers["inactive"] = CleanupProviderConfig{APIKeyEnv: "INACTIVE_KEY"}

	environment := []string{
		"PATH=/usr/bin", "HOME=/home/test", "XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "GNUPGHOME=/home/test/.gnupg",
		"GPG_TTY=/dev/pts/1", "SSH_AUTH_SOCK=/run/user/1000/agent", "PASSWORD_STORE_DIR=/home/test/.password-store",
		"PRIMARY_KEY=primary", "SHARED_KEY=first", "SHARED_KEY=second",
		"inactive_key=lower", "INACTIVE_KEY=upper", "Inactive_Key=unconfigured-case",
	}
	filtered := CredentialHelperEnvironment(cfg, environment)
	want := strings.Join([]string{
		"PATH=/usr/bin", "HOME=/home/test", "XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "GNUPGHOME=/home/test/.gnupg",
		"GPG_TTY=/dev/pts/1", "SSH_AUTH_SOCK=/run/user/1000/agent", "PASSWORD_STORE_DIR=/home/test/.password-store",
		"Inactive_Key=unconfigured-case",
	}, "|")
	if got := strings.Join(filtered, "|"); got != want {
		t.Fatalf("credential helper environment = %q, want %q", got, want)
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

func errorChainContains(err error, substring string) bool {
	for err != nil {
		if strings.Contains(err.Error(), substring) {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}
