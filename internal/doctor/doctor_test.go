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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/policy"
	"git.2027a.net/2027a/murmur/internal/sysexec"
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

type sourceCheckerFunc func(context.Context, string) error

func (f sourceCheckerFunc) Check(ctx context.Context, source string) error { return f(ctx, source) }

type providerCheckerFunc func(context.Context, string, config.ProviderConfig, string) error

func (f providerCheckerFunc) Check(ctx context.Context, name string, provider config.ProviderConfig, secret string) error {
	return f(ctx, name, provider, secret)
}

type secretResolverFunc func(context.Context, config.CredentialConfig) (string, error)

func (f secretResolverFunc) ResolveCredentials(ctx context.Context, credentials config.CredentialConfig) (string, error) {
	return f(ctx, credentials)
}

type commandRunnerFunc func(context.Context, string, []string) ([]byte, error)

func (f commandRunnerFunc) Run(ctx context.Context, name string, args []string) ([]byte, error) {
	return f(ctx, name, args)
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

func TestDoctorRejectsCrossOriginHealthBeforeCredentialAndNetwork(t *testing.T) {
	cfg := config.Defaults()
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.APIKeyEnv = "TEST_KEY"
	provider.HealthEnabled = true
	provider.HealthEndpoint = "http://127.0.0.1:9000/health"
	cfg.STT.Providers[cfg.STT.Provider] = provider
	providers := &fakeProviders{}
	secrets := &rejectingSecrets{}
	report := Run(context.Background(), cfg, baseDependencies(providers, secrets))
	if report.OK || len(report.Checks) != 1 || report.Checks[0].Name != "configuration et policy" ||
		!strings.Contains(report.Checks[0].Detail, "même origine") {
		t.Fatalf("report = %#v", report)
	}
	if secrets.calls != 0 || len(providers.names) != 0 {
		t.Fatalf("credential calls = %d, provider checks = %v", secrets.calls, providers.names)
	}
}

func TestDoctorMatchesConfigPreflightWithoutExternalChecks(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*config.Config)
	}{
		{"empty model", func(cfg *config.Config) {
			provider := cfg.STT.Providers[cfg.STT.Provider]
			provider.Model = ""
			cfg.STT.Providers[cfg.STT.Provider] = provider
		}},
		{"unknown shortcut", func(cfg *config.Config) { cfg.Sink.PasteShortcuts = []string{"super+v"} }},
		{"non-persistent clipboard sink", func(cfg *config.Config) {
			cfg.Sink.Mode = "clipboard"
			cfg.Sink.ClipboardMode = "none"
		}},
		{"duration above maximum", func(cfg *config.Config) { cfg.Plugin.TimeoutS = config.MaximumDurationSeconds + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			test.edit(&cfg)
			want := config.Validate(cfg)
			if want == nil {
				t.Fatal("test configuration is valid")
			}
			var externalCalls atomic.Int32
			secrets := &rejectingSecrets{}
			providers := &fakeProviders{}
			report := Run(context.Background(), cfg, Dependencies{
				LookPath: func(string) (string, error) { externalCalls.Add(1); return "", errors.New("called") },
				Runtime:  func() error { externalCalls.Add(1); return nil },
				File:     func(string) error { externalCalls.Add(1); return nil },
				Source:   &fakeSource{}, Providers: providers, Secrets: secrets,
			})
			if report.OK || len(report.Checks) != 1 || report.Checks[0].Detail != want.Error() {
				t.Fatalf("report = %#v, want detail %q", report, want)
			}
			if externalCalls.Load() != 0 || secrets.calls != 0 || len(providers.names) != 0 {
				t.Fatalf("external=%d credentials=%d providers=%v", externalCalls.Load(), secrets.calls, providers.names)
			}
		})
	}
}

func TestDefaultDiagnosticExecutorFiltersCredentialEnvironment(t *testing.T) {
	t.Setenv("MURMUR_DOCTOR_SECRET", "must-not-reach-helper")
	t.Setenv("MURMUR_DOCTOR_SAFE", "safe-value")
	cfg := config.Defaults()
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.APIKeyEnv = "MURMUR_DOCTOR_SECRET"
	cfg.STT.Providers[cfg.STT.Provider] = provider

	dependencies := withDefaults(cfg, Dependencies{})
	resolver, ok := dependencies.Secrets.(config.SecretResolver)
	if !ok {
		t.Fatalf("credential resolver type = %T", dependencies.Secrets)
	}
	runner, ok := resolver.Runner.(secretRunner)
	if !ok {
		t.Fatalf("credential runner type = %T", resolver.Runner)
	}
	for label, candidate := range map[string]sysexec.Executor{
		"diagnostic": dependencies.Executor,
		"credential": runner.executor,
	} {
		executor, ok := candidate.(sysexec.OSExecutor)
		if !ok {
			t.Fatalf("%s executor type = %T", label, candidate)
		}
		environment := strings.Join(executor.Env, "\n")
		if strings.Contains(environment, "MURMUR_DOCTOR_SECRET=must-not-reach-helper") {
			t.Fatalf("credential environment leaked to %s executor", label)
		}
		if !strings.Contains(environment, "MURMUR_DOCTOR_SAFE=safe-value") {
			t.Fatalf("usual environment missing from %s executor", label)
		}
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

func TestDoctorTimesOutEachExternalCheckAndContinues(t *testing.T) {
	const timeout = 20 * time.Millisecond
	block := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	for _, test := range []struct {
		name      string
		checkName string
		configure func(*config.Config, *Dependencies)
	}{
		{
			name: "pw-cli", checkName: "source audio",
			configure: func(cfg *config.Config, deps *Dependencies) {
				cfg.Audio.Source = "alsa_input.blocked"
				deps.Source = nil
				deps.Executor = &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, _ string, _ []string, _ []byte) ([]byte, error) {
					return nil, block(ctx)
				}}
			},
		},
		{
			name: "credential helper", checkName: "credential STT local",
			configure: func(cfg *config.Config, deps *Dependencies) {
				provider := cfg.STT.Providers[cfg.STT.Provider]
				provider.APIKeyCommand = []string{"blocked-secret"}
				cfg.STT.Providers[cfg.STT.Provider] = provider
				deps.Secrets = secretResolverFunc(func(ctx context.Context, _ config.CredentialConfig) (string, error) {
					return "", block(ctx)
				})
			},
		},
		{
			name: "provider health", checkName: "sante STT local",
			configure: func(cfg *config.Config, deps *Dependencies) {
				provider := cfg.STT.Providers[cfg.STT.Provider]
				provider.HealthEnabled = true
				provider.HealthEndpoint = "/health"
				cfg.STT.Providers[cfg.STT.Provider] = provider
				deps.Providers = providerCheckerFunc(func(ctx context.Context, _ string, _ config.ProviderConfig, _ string) error {
					return block(ctx)
				})
			},
		},
		{
			name: "D-Bus", checkName: "UI notifications D-Bus",
			configure: func(cfg *config.Config, deps *Dependencies) {
				cfg.UI.Mode = "notify"
				deps.Notifications = block
			},
		},
		{
			name: "OSD helper", checkName: "UI helper OSD",
			configure: func(cfg *config.Config, deps *Dependencies) {
				cfg.UI.Mode = "osd"
				deps.Executor = &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, _ string, _ []string, _ []byte) ([]byte, error) {
					return nil, block(ctx)
				}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.UI.Mode = "none"
			cfg.UI.Sound = true
			deps := baseDependencies(&fakeProviders{}, &staticSecrets{})
			deps.ExternalTimeout = timeout
			var followingCheck atomic.Bool
			deps.LookPath = func(name string) (string, error) {
				if name == "pw-play" {
					followingCheck.Store(true)
				}
				return "/bin/" + name, nil
			}
			test.configure(&cfg, &deps)

			started := time.Now()
			report := Run(context.Background(), cfg, deps)
			if elapsed := time.Since(started); elapsed >= time.Second {
				t.Fatalf("doctor elapsed = %v", elapsed)
			}
			if report.OK {
				t.Fatalf("report = %#v", report)
			}
			var failed *Check
			for i := range report.Checks {
				if report.Checks[i].Name == test.checkName {
					failed = &report.Checks[i]
					break
				}
			}
			if failed == nil || failed.Status != Failed || !strings.Contains(failed.Detail, context.DeadlineExceeded.Error()) {
				t.Fatalf("timed check = %#v in %#v", failed, report)
			}
			if !followingCheck.Load() {
				t.Fatal("check suivant non exécuté")
			}
		})
	}
}

func TestDoctorCredentialKeepsShorterInternalTimeout(t *testing.T) {
	cfg := config.Defaults()
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.APIKeyCommand = []string{"blocked-secret"}
	cfg.STT.Providers[cfg.STT.Provider] = provider

	const internalTimeout = 20 * time.Millisecond
	var runnerBudget time.Duration
	resolver := config.SecretResolver{
		CommandTimeout: internalTimeout,
		Runner: commandRunnerFunc(func(ctx context.Context, _ string, _ []string) ([]byte, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, errors.New("helper sans deadline")
			}
			runnerBudget = time.Until(deadline)
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	}
	deps := baseDependencies(&fakeProviders{}, resolver)
	deps.ExternalTimeout = 500 * time.Millisecond
	started := time.Now()
	report := Run(context.Background(), cfg, deps)
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("doctor elapsed = %v, internal timeout was not preserved", elapsed)
	}
	credential := findCheck(t, report, "credential STT local")
	if credential.Status != Failed || runnerBudget <= 0 || runnerBudget > internalTimeout+10*time.Millisecond {
		t.Fatalf("credential check=%#v runner budget=%v", credential, runnerBudget)
	}
}

func TestDoctorCredentialPropagatesGlobalCancellationAndContinues(t *testing.T) {
	cfg := config.Defaults()
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.APIKeyCommand = []string{"blocked-secret"}
	cfg.STT.Providers[cfg.STT.Provider] = provider
	cfg.UI.Sound = true

	started := make(chan struct{})
	var startOnce sync.Once
	deps := baseDependencies(&fakeProviders{}, secretResolverFunc(func(ctx context.Context, _ config.CredentialConfig) (string, error) {
		startOnce.Do(func() { close(started) })
		<-ctx.Done()
		return "", ctx.Err()
	}))
	var followingCheck atomic.Bool
	deps.LookPath = func(name string) (string, error) {
		if name == "pw-play" {
			followingCheck.Store(true)
		}
		return "/bin/" + name, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Report, 1)
	go func() { done <- Run(ctx, cfg, deps) }()
	<-started
	cancel()

	select {
	case report := <-done:
		credential := findCheck(t, report, "credential STT local")
		if credential.Status != Failed || !strings.Contains(credential.Detail, context.Canceled.Error()) {
			t.Fatalf("credential check=%#v", credential)
		}
		if !followingCheck.Load() {
			t.Fatal("check suivant non exécuté après annulation du credential")
		}
	case <-time.After(time.Second):
		t.Fatal("doctor n'a pas propagé l'annulation du credential")
	}
}

func findCheck(t *testing.T, report Report, name string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %q absent de %#v", name, report)
	return Check{}
}

func TestDoctorGlobalContextCancelsExternalChecks(t *testing.T) {
	cfg := config.Defaults()
	cfg.Audio.Source = "alsa_input.blocked"
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.HealthEnabled = true
	provider.HealthEndpoint = "/health"
	cfg.STT.Providers[cfg.STT.Provider] = provider
	cfg.UI.Mode = "notify"

	started := make(chan struct{})
	var sourceCalls, providerCalls, notificationCalls atomic.Int32
	blocking := func(ctx context.Context, calls *atomic.Int32) error {
		calls.Add(1)
		if calls == &sourceCalls {
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	deps := baseDependencies(&fakeProviders{}, &staticSecrets{})
	deps.Source = sourceCheckerFunc(func(ctx context.Context, _ string) error { return blocking(ctx, &sourceCalls) })
	deps.Providers = providerCheckerFunc(func(ctx context.Context, _ string, _ config.ProviderConfig, _ string) error {
		return blocking(ctx, &providerCalls)
	})
	deps.Notifications = func(ctx context.Context) error { return blocking(ctx, &notificationCalls) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Report, 1)
	go func() { done <- Run(ctx, cfg, deps) }()
	<-started
	cancel()

	select {
	case report := <-done:
		if report.OK || sourceCalls.Load() != 1 || providerCalls.Load() != 1 || notificationCalls.Load() != 1 {
			t.Fatalf("report=%#v calls source=%d provider=%d notifications=%d", report, sourceCalls.Load(), providerCalls.Load(), notificationCalls.Load())
		}
		for _, name := range []string{"source audio", "sante STT local", "UI notifications D-Bus"} {
			found := false
			for _, check := range report.Checks {
				if check.Name == name {
					found = true
					if !strings.Contains(check.Detail, context.Canceled.Error()) {
						t.Fatalf("check %q = %#v", name, check)
					}
				}
			}
			if !found {
				t.Fatalf("check %q absent de %#v", name, report)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("doctor n'a pas propagé l'annulation globale")
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

func TestOutputFileCheckProbesSiblingAtomicallyWithoutTouchingDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o400); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOutputFile(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing" || after.ModTime() != before.ModTime() || after.Size() != before.Size() {
		t.Fatalf("destination touched: before=%#v after=%#v data=%q", before, after, data)
	}
	assertNoDoctorProbe(t, dir)
}

func TestOutputFileCheckRejectsUnsafeDestinations(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, dir} {
		if err := checkOutputFile(path); err == nil {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
	assertNoDoctorProbe(t, dir)
}

func assertNoDoctorProbe(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".murmur-doctor-") {
			t.Fatalf("doctor probe left behind: %s", entry.Name())
		}
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
