package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/policy"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

type fakeSource struct {
	called bool
	source string
}

func (f *fakeSource) Check(_ context.Context, source string) error {
	f.called = true
	f.source = source
	return nil
}

type fakeProviders struct {
	mu    sync.Mutex
	names []string
}

func (f *fakeProviders) Check(_ context.Context, name string, _ config.ProviderConfig, _ string) error {
	f.mu.Lock()
	f.names = append(f.names, name)
	f.mu.Unlock()
	return nil
}

type rejectingSecrets struct{ calls int }

func (f *rejectingSecrets) ResolveCredentials(context.Context, config.CredentialConfig) (string, error) {
	f.calls++
	return "", errors.New("credential should not be requested")
}

func TestAcceptance_14_DoctorIsStrictlyConditional(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sink.Mode = "stdout"
	cfg.Sink.ClipboardMode = "none"
	cfg.UI.Mode = "none"
	cfg.STT.Providers["unused-cloud"] = config.ProviderConfig{
		Type: "openai-compatible", BaseURL: "https://example.com/v1", Model: "cloud",
		TimeoutS: 10, Zone: "internet", APIKeyEnv: "UNUSED_CLOUD_KEY",
	}

	var looked []string
	source := &fakeSource{}
	providers := &fakeProviders{}
	secrets := &rejectingSecrets{}
	report := Run(context.Background(), cfg, Dependencies{
		LookPath: func(name string) (string, error) {
			looked = append(looked, name)
			if name == "pw-record" {
				return "/usr/bin/pw-record", nil
			}
			return "", errors.New("missing")
		},
		Runtime:       func() error { return nil },
		Notifications: func(context.Context) error { return errors.New("must not be called") },
		Source:        source,
		Providers:     providers,
		Secrets:       secrets,
	})
	if !report.OK {
		t.Fatalf("report = %#v", report)
	}
	if !reflect.DeepEqual(looked, []string{"pw-record"}) {
		t.Fatalf("looked up tools = %v", looked)
	}
	if secrets.calls != 0 {
		t.Fatalf("secret calls = %d", secrets.calls)
	}
	if !source.called || len(providers.names) != 0 {
		t.Fatalf("source=%v providers=%v", source.called, providers.names)
	}
	for _, check := range report.Checks {
		if check.Name == "sink: wl-copy" || check.Name == "sink: wtype" || check.Name == "UI notifications D-Bus" {
			t.Fatalf("unexpected check: %#v", check)
		}
	}
}

func TestDoctorChecksOnlyReferencedProviders(t *testing.T) {
	cfg := config.Defaults()
	cfg.STT.Fallback = "fallback"
	primary := cfg.STT.Providers[cfg.STT.Provider]
	primary.HealthEnabled = true
	primary.HealthEndpoint = "/health"
	cfg.STT.Providers[cfg.STT.Provider] = primary
	cfg.STT.Providers["fallback"] = config.ProviderConfig{
		Type: "openai-compatible", BaseURL: "http://127.0.0.1:9000/v1", Model: "fallback",
		Language: "fr", TimeoutS: 10, Zone: "local", APIKeyEnv: "FALLBACK_KEY",
		HealthEnabled: true, HealthEndpoint: "/health",
	}
	cfg.Cleanup.Enabled = true
	cfg.Cleanup.Provider = "cleanup"
	cfg.Cleanup.Providers["cleanup"] = config.CleanupProviderConfig{
		Type: "openai-compatible", BaseURL: "http://127.0.0.1:9001/v1", Model: "cleanup",
		TimeoutS: 10, Zone: "local",
	}

	providers := &fakeProviders{}
	secrets := &staticSecrets{value: "secret"}
	report := Run(context.Background(), cfg, baseDependencies(providers, secrets))
	if !report.OK {
		t.Fatalf("report = %#v", report)
	}
	if !reflect.DeepEqual(providers.names, []string{"local", "fallback"}) {
		t.Fatalf("provider checks = %v", providers.names)
	}
	if secrets.calls != 1 {
		t.Fatalf("secret calls = %d", secrets.calls)
	}
}

func TestDefaultDiagnosticExecutorFiltersCredentialEnvironment(t *testing.T) {
	t.Setenv("MURMUR_DOCTOR_SECRET", "must-not-reach-helper")
	cfg := config.Defaults()
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.APIKeyEnv = "MURMUR_DOCTOR_SECRET"
	cfg.STT.Providers[cfg.STT.Provider] = provider

	dependencies := withDefaults(cfg, Dependencies{})
	executor, ok := dependencies.Executor.(sysexec.OSExecutor)
	if !ok {
		t.Fatalf("diagnostic executor type = %T", dependencies.Executor)
	}
	for _, assignment := range executor.Env {
		if assignment == "MURMUR_DOCTOR_SECRET=must-not-reach-helper" {
			t.Fatal("credential environment leaked to diagnostic executor")
		}
	}
	if dependencies.Secrets == nil {
		t.Fatal("credential resolver was not configured")
	}
}

type staticSecrets struct {
	value string
	calls int
}

func (f *staticSecrets) ResolveCredentials(context.Context, config.CredentialConfig) (string, error) {
	f.calls++
	return f.value, nil
}

func TestDoctorConditionalSinkUIAndFileChecks(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sink.Mode = "file"
	cfg.Sink.ClipboardMode = "none"
	cfg.Sink.File = filepath.Join(t.TempDir(), "result.txt")
	cfg.UI.Mode = "osd"
	cfg.UI.Sound = true

	var looked []string
	fileChecked := ""
	deps := baseDependencies(&fakeProviders{}, &staticSecrets{})
	deps.LookPath = func(name string) (string, error) {
		looked = append(looked, name)
		if name == "pw-record" || name == "murmur-osd" || name == "paplay" {
			return "/bin/" + name, nil
		}
		return "", errors.New("missing")
	}
	deps.File = func(path string) error { fileChecked = path; return nil }
	report := Run(context.Background(), cfg, deps)
	if !report.OK {
		t.Fatalf("report = %#v", report)
	}
	wantTools := []string{"pw-record", "murmur-osd", "pw-play", "paplay"}
	if !reflect.DeepEqual(looked, wantTools) {
		t.Fatalf("looked = %v, want %v", looked, wantTools)
	}
	if fileChecked != cfg.Sink.File {
		t.Fatalf("file check = %q", fileChecked)
	}
}

func TestReportHumanAndJSON(t *testing.T) {
	report := Report{Version: 1, OK: false, Checks: []Check{
		{Name: "one", Status: OK},
		{Name: "two", Status: Failed, Detail: "absent"},
	}}
	var human bytes.Buffer
	if err := report.WriteHuman(&human); err != nil {
		t.Fatal(err)
	}
	if human.String() != "[OK] one\n[ECHEC] two: absent\n" {
		t.Fatalf("human output = %q", human.String())
	}
	var encoded bytes.Buffer
	if err := report.WriteJSON(&encoded); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, report) {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestOutputFileCheckDoesNotCreateFinalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "output.txt")
	if err := checkOutputFile(path); err == nil {
		t.Fatal("missing parent accepted")
	}
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkOutputFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("final file was created: %v", err)
	}
}

func TestPipeWireSourceCheckIsConditionalAndNonCapturing(t *testing.T) {
	executor := &sysexec.FakeExecutor{}
	checker := pipeWireSourceChecker{executor: executor}
	if err := checker.Check(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if calls := executor.Calls(); len(calls) != 0 {
		t.Fatalf("default source calls = %#v", calls)
	}
	if err := checker.Check(context.Background(), "alsa_input.test"); err != nil {
		t.Fatal(err)
	}
	calls := executor.Calls()
	if len(calls) != 1 || calls[0].Name != "pw-cli" || !reflect.DeepEqual(calls[0].Args, []string{"info", "alsa_input.test"}) {
		t.Fatalf("explicit source calls = %#v", calls)
	}
}

func TestProviderHealthRejectsRedirectAndKeepsAuth(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	checker := httpProviderChecker{}
	err := checker.Check(context.Background(), "test", config.ProviderConfig{
		BaseURL: server.URL + "/v1", TimeoutS: 1, Zone: policy.Local.String(),
		HealthEnabled: true, HealthEndpoint: "/health",
	}, "secret")
	if err == nil || redirected.Load() != 0 {
		t.Fatalf("Check() error = %v, redirect calls = %d", err, redirected.Load())
	}
}

func baseDependencies(providers ProviderChecker, secrets SecretResolver) Dependencies {
	return Dependencies{
		LookPath: func(string) (string, error) { return "/bin/tool", nil },
		Runtime:  func() error { return nil },
		File:     func(string) error { return nil },
		Notifications: func(context.Context) error {
			return nil
		},
		Source:    &fakeSource{},
		Providers: providers,
		Secrets:   secrets,
		Executor:  &sysexec.FakeExecutor{},
	}
}
