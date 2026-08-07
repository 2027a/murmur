package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/ipc"
	"git.2027a.net/mathieu/murmur/internal/session"
	"git.2027a.net/mathieu/murmur/internal/sink"
	"git.2027a.net/mathieu/murmur/internal/stt"
	"git.2027a.net/mathieu/murmur/internal/textpipe"
	"git.2027a.net/mathieu/murmur/internal/xdg"
)

func privateCLITempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUsageErrorsReturnTwo(t *testing.T) {
	for _, args := range [][]string{{}, {"unknown"}, {"stop", "extra"}, {"status", "--wat"}, {"start", "--clipboard-only", "--clipboard-only"}, {"toggle", "--clipboard-only", "--clipboard-only"}, {"transcribe"}, {"cleanup", "extra"}, {"dictate", "--sink", "paste"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Fatalf("Run(%v)=%d stderr=%q", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "Usage: murmur") {
			t.Fatalf("Run(%v) stderr=%q", args, stderr.String())
		}
	}
}

func TestDictateDefaultsToStdoutWhenSinkIsOmitted(t *testing.T) {
	options, err := parseDictateOptions([]string{"--json"})
	if err != nil {
		t.Fatal(err)
	}
	if options.sinkMode != "stdout" || !options.jsonOutput || options.eventOutput {
		t.Fatalf("options=%#v", options)
	}

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "dictate [--sink stdout|file]") ||
		!strings.Contains(stdout.String(), "stdout par défaut") {
		t.Fatalf("help=%q", stdout.String())
	}
}

func TestDictateFileRequiresConfiguredPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", privateCLITempDir(t))
	var stdout, stderr bytes.Buffer
	code := (App{Stdin: strings.NewReader("")}).Run(
		context.Background(), []string{"dictate", "--sink", "file", "--json"}, &stdout, &stderr,
	)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success || result.Error == nil || result.Error.Class != errs.ConfigInvalid || result.Error.Component != "config" ||
		!strings.Contains(result.Error.Message, "sink.file") {
		t.Fatalf("result=%#v", result)
	}
}

func TestCleanupDisabledPassesStdinThrough(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", privateCLITempDir(t))
	input := "texte brut\nconservé"
	var stdout, stderr bytes.Buffer
	code := (App{Stdin: strings.NewReader(input)}).Run(context.Background(), []string{"cleanup", "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.TranscriptRaw != input || result.TranscriptFinal != input || len(result.Warnings) != 1 || result.Warnings[0].Component != "cleanup" {
		t.Fatalf("result=%#v", result)
	}
}

func TestCleanupMissingConfiguredProviderIsStructuredFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[cleanup]\nenabled = true\nprovider = \"missing\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := (App{ConfigPath: path, Stdin: strings.NewReader("secret input")}).Run(context.Background(), []string{"cleanup", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success || result.Error == nil || result.Error.Class != "config_invalid" || strings.Contains(result.Error.Message, "secret input") || result.TranscriptRaw != "secret input" || result.TranscriptFinal != "secret input" {
		t.Fatalf("result=%#v", result)
	}
}

func TestCleanupHTTP503PreservesClassInJSONAndEvents(t *testing.T) {
	const canary = "MURMUR-SENSITIVE-CLEANUP-CLI-CANARY"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, canary, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf(`[cleanup]
enabled = true
provider = "test"

[cleanup.providers.test]
type = "openai-compatible"
base_url = %q
model = "test"
timeout_s = 1
zone = "local"
`, server.URL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := (App{ConfigPath: path, Stdin: strings.NewReader("safe input")}).Run(
		context.Background(), []string{"cleanup", "--json", "--events"}, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.TranscriptFinal != "safe input" || len(result.Warnings) != 1 ||
		result.Warnings[0].Class != errs.ServerError || result.Warnings[0].Component != "cleanup" {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(stdout.String(), canary) || strings.Contains(stderr.String(), canary) {
		t.Fatalf("sensitive HTTP body leaked: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	decoder := json.NewDecoder(&stderr)
	var degraded bool
	for {
		var event events.Event
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		degraded = degraded || event.Type == events.Degraded && event.ErrorClass == errs.ServerError && event.Component == "cleanup"
	}
	if !degraded {
		t.Fatal("classified cleanup degradation event missing")
	}
}

func TestComposableCleanupFiltersCredentialHelperEnvironment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "texte propre"}}},
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	dump := filepath.Join(dir, "helper-environment")
	helper := filepath.Join(dir, "credential-helper")
	writeCLITestExecutable(t, helper, "#!/bin/sh\nenv > \"$1\"\nprintf token\n")
	path := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf(`[stt]
fallback = "fallback"

[stt.providers.fallback]
type = "openai-compatible"
base_url = "http://127.0.0.1:8000/v1"
model = "fallback"
language = "fr"
timeout_s = 10
zone = "local"
api_key_env = "MURMUR_CLI_FALLBACK_KEY"

[stt.providers.inactive]
type = "openai-compatible"
base_url = "http://127.0.0.1:8000/v1"
model = "inactive"
language = "fr"
timeout_s = 10
zone = "local"
api_key_env = "MURMUR_CLI_INACTIVE_KEY"

[cleanup]
enabled = true
provider = "cleanup"

[cleanup.providers.cleanup]
type = "openai-compatible"
base_url = %q
model = "cleanup"
timeout_s = 10
zone = "local"
api_key_command = [%q, %q]

[cleanup.providers.inactive]
type = "openai-compatible"
base_url = "http://127.0.0.1:8000/v1"
model = "inactive"
timeout_s = 10
zone = "local"
api_key_env = "MURMUR_CLI_CLEANUP_KEY"
`, server.URL+"/v1", helper, dump)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"MURMUR_CLI_FALLBACK_KEY": "fallback-secret",
		"MURMUR_CLI_INACTIVE_KEY": "inactive-secret",
		"MURMUR_CLI_CLEANUP_KEY":  "cleanup-secret",
		"MURMUR_CLI_SAFE":         "safe-value",
	} {
		t.Setenv(name, value)
	}

	var stdout, stderr bytes.Buffer
	code := (App{ConfigPath: path, Stdin: strings.NewReader("texte brut")}).Run(
		context.Background(), []string{"cleanup", "--json"}, &stdout, &stderr,
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	environment := string(data)
	for _, name := range []string{"MURMUR_CLI_FALLBACK_KEY", "MURMUR_CLI_INACTIVE_KEY", "MURMUR_CLI_CLEANUP_KEY"} {
		if strings.Contains(environment, name+"=") {
			t.Fatalf("credential %s reached composable helper", name)
		}
	}
	if !strings.Contains(environment, "MURMUR_CLI_SAFE=safe-value\n") {
		t.Fatalf("usual environment missing from composable helper: %q", environment)
	}
}

func TestUnsafeConfigIsRejectedBeforeHelperNetworkAndCapture(t *testing.T) {
	dir := t.TempDir()
	tools := filepath.Join(dir, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	helperMarker := filepath.Join(dir, "helper-called")
	captureMarker := filepath.Join(dir, "capture-called")
	helper := filepath.Join(tools, "credential-helper")
	pwRecord := filepath.Join(tools, "pw-record")
	writeCLITestExecutable(t, helper, "#!/bin/sh\nprintf called >"+helperMarker+"\nprintf token\n")
	writeCLITestExecutable(t, pwRecord, "#!/bin/sh\nprintf called >"+captureMarker+"\nexit 1\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	runtimeBase := filepath.Join(dir, "runtime")
	if err := os.Mkdir(runtimeBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	path := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf(`[stt.providers.local]
type = "openai-compatible"
base_url = %q
model = "test"
language = "fr"
timeout_s = 1
zone = "local"
health_enabled = true
health_endpoint = "/health"
api_key_command = [%q]
`, server.URL, helper)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		args  []string
		stdin io.Reader
	}{
		{name: "doctor", args: []string{"doctor", "--json"}},
		{name: "dictate", args: []string{"dictate", "--sink", "stdout", "--json"}, stdin: strings.NewReader("")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := (App{ConfigPath: path, Stdin: test.stdin}).Run(context.Background(), test.args, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
	if _, err := os.Stat(helperMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper exécuté avant refus de configuration: %v", err)
	}
	if _, err := os.Stat(captureMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture exécutée avant refus de configuration: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requêtes réseau avant refus de configuration: %d", requests.Load())
	}
}

func TestStartRejectsTransientPasteBeforeCapture(t *testing.T) {
	dir := t.TempDir()
	tools := filepath.Join(dir, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	captureMarker := filepath.Join(dir, "capture-called")
	writeCLITestExecutable(t, filepath.Join(tools, "pw-record"), "#!/bin/sh\nprintf called >"+captureMarker+"\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sink]\nmode = \"paste\"\nclipboard_mode = \"transient\"\npaste_shortcuts = [\"ctrl+v\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := (App{ConfigPath: path}).Run(context.Background(), []string{"start"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "ne confirme pas la consommation") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(captureMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture exécutée avant refus transient+paste: %v", err)
	}
}

func TestCleanupInputLimitIsStructured(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", privateCLITempDir(t))
	input := strings.Repeat("x", maxCleanupInputBytes+1)
	var stdout, stderr bytes.Buffer
	code := (App{Stdin: strings.NewReader(input)}).Run(context.Background(), []string{"cleanup", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var result commandResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || result.Error.Class != errs.Unavailable || result.Error.Component != "stdin" || result.TranscriptRaw != input || result.TranscriptFinal != input {
		t.Fatalf("result=%#v raw-bytes=%d final-bytes=%d", result, len(result.TranscriptRaw), len(result.TranscriptFinal))
	}
}

func TestComposableTranscriptionExposesMixedProviderAndSegmentDetails(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := cliProvider{name: "primary", transcribe: func(context.Context, stt.Request) (stt.Result, error) {
		if primaryCalls.Add(1) == 2 {
			return stt.Result{}, errs.New(errs.ServerError, "fake", "down")
		}
		return stt.Result{Text: "primary"}, nil
	}}
	fallback := cliProvider{name: "fallback", transcribe: func(context.Context, stt.Request) (stt.Result, error) {
		return stt.Result{Text: "fallback"}, nil
	}}
	chain, err := stt.NewChain(primary, fallback, stt.MultiOptions{AllowMixedProviders: true, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := textpipe.New(textpipe.Options{CommandPack: "fr", STTLanguage: "fr"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.STT.Provider = "primary"
	cfg.STT.ChunkSeconds = 1
	cfg.STT.Providers = map[string]config.ProviderConfig{
		"primary":  {Model: "primary-model", Language: "fr"},
		"fallback": {Model: "fallback-model", Language: "en"},
	}
	var eventOutput bytes.Buffer
	emitter := newCommandEmitter(true, &eventOutput, "transcribe")
	result, err := executeTranscriptionWith(context.Background(), cfg, cliWAV(t, 2*time.Second), chain, pipeline, emitter, "transcribe")
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != stt.ProviderMixed || result.Model != "" || result.Language != "" || result.TranscriptRaw != "primary fallback" {
		t.Fatalf("result=%#v", result)
	}
	wantSegments := []commandSegment{
		{Index: 0, Provider: "primary", Model: "primary-model", Language: "fr", Transcript: "primary"},
		{Index: 1, Provider: "fallback", Model: "fallback-model", Language: "en", Transcript: "fallback"},
	}
	if !reflect.DeepEqual(result.Segments, wantSegments) {
		t.Fatalf("segments=%#v, want %#v", result.Segments, wantSegments)
	}
	decoder := json.NewDecoder(&eventOutput)
	var selected, processing bool
	for {
		var event events.Event
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		selected = selected || event.Type == events.ProviderSelected && event.Provider == stt.ProviderMixed
		processing = processing || event.Type == events.StateChanged && event.State == "processing" && event.Provider == stt.ProviderMixed
	}
	if !selected || !processing {
		t.Fatalf("mixed global events missing: selected=%v processing=%v output=%s", selected, processing, eventOutput.String())
	}
}

func TestComposableBudgetFailureReportsFallbackProvider(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := cliProvider{name: "primary", transcribe: func(context.Context, stt.Request) (stt.Result, error) {
		if primaryCalls.Add(1) == 2 {
			return stt.Result{}, errs.New(errs.ServerError, "fake", "down")
		}
		return stt.Result{Text: "aaaa"}, nil
	}}
	fallback := cliProvider{name: "fallback", transcribe: func(context.Context, stt.Request) (stt.Result, error) {
		return stt.Result{Text: "bbbb"}, nil
	}}
	chain, err := stt.NewChain(primary, fallback, stt.MultiOptions{MaxTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := textpipe.New(textpipe.Options{CommandPack: "fr", STTLanguage: "fr"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.STT.Provider = "primary"
	cfg.STT.Fallback = "fallback"
	cfg.STT.ChunkSeconds = 1
	cfg.STT.Providers = map[string]config.ProviderConfig{
		"primary":  {Model: "primary-model", Language: "fr"},
		"fallback": {Model: "fallback-model", Language: "fr"},
	}
	result, err := executeTranscriptionWith(
		context.Background(), cfg, cliWAV(t, 2*time.Second), chain, pipeline,
		newCommandEmitter(false, io.Discard, "transcribe"), "transcribe",
	)
	if !stt.IsLimitExceeded(err) {
		t.Fatalf("error=%v", err)
	}
	if result.Provider != "fallback" || result.Model != "fallback-model" || result.Language != "fr" {
		t.Fatalf("result=%#v", result)
	}
}

func TestComposablePipelineWarningsPreserveClassesWithoutSensitiveErrors(t *testing.T) {
	const canary = "MURMUR-SENSITIVE-CLI-CANARY"
	for _, class := range []errs.Class{errs.ServerError, errs.Timeout, errs.AuthFailed, errs.PolicyBlocked, errs.Unavailable} {
		t.Run(string(class), func(t *testing.T) {
			chain, err := stt.NewChain(cliProvider{name: "local", transcribe: func(context.Context, stt.Request) (stt.Result, error) {
				return stt.Result{Text: "texte"}, nil
			}}, nil, stt.MultiOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pipeline, err := textpipe.New(textpipe.Options{
				CommandPack:     "fr",
				CleanupProvider: "cleanup",
				Cleanup: textpipe.CleanupFunc(func(context.Context, string, textpipe.Protection) (string, error) {
					return "", errs.Wrap(class, "unsafe/"+canary, "unsafe "+canary, errors.New("cause "+canary))
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			var eventOutput bytes.Buffer
			emitter := newCommandEmitter(true, &eventOutput, "transcribe")
			result, err := executeTranscriptionWith(context.Background(), config.Defaults(), cliWAV(t, time.Second), chain, pipeline, emitter, "transcribe")
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Warnings) != 1 || result.Warnings[0].Class != class ||
				result.Warnings[0].Provider != "cleanup" || result.Warnings[0].Component != "cleanup" {
				t.Fatalf("warnings=%#v", result.Warnings)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte(canary)) {
				t.Fatalf("sensitive pipeline error leaked in %s", encoded)
			}
			decoder := json.NewDecoder(&eventOutput)
			var degraded bool
			for {
				var event events.Event
				if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				degraded = degraded || event.Type == events.Degraded && event.Provider == "cleanup" &&
					event.ErrorClass == class && event.Component == "cleanup"
			}
			if !degraded {
				t.Fatalf("classified degraded event missing from %s", class)
			}
		})
	}
}

func TestDictateRecorderFailureCancelsStdinAndAborts(t *testing.T) {
	input := newBlockingStdin()
	recording := newDictateRecording()
	result := make(chan error, 1)
	go func() {
		result <- (App{Stdin: input}).waitForDictateStop(context.Background(), recording)
	}()

	waitChannel(t, input.started, "stdin read")
	recording.fail(errs.New(errs.AudioFailed, "audio/test", "recorder stopped"))
	select {
	case err := <-result:
		if errs.Classify(err) != errs.AudioFailed {
			t.Fatalf("error=%v class=%s", err, errs.Classify(err))
		}
	case <-time.After(time.Second):
		t.Fatal("recorder failure did not stop dictate wait")
	}
	if recording.aborts.Load() != 1 {
		t.Fatalf("Abort calls=%d", recording.aborts.Load())
	}
	waitChannel(t, input.cancelled, "stdin cancellation")
}

func TestDictateEOFAndContextRemainNormalStops(t *testing.T) {
	t.Run("EOF", func(t *testing.T) {
		recording := newDictateRecording()
		if err := (App{Stdin: strings.NewReader("")}).waitForDictateStop(context.Background(), recording); err != nil {
			t.Fatal(err)
		}
		if recording.aborts.Load() != 0 {
			t.Fatalf("Abort calls=%d", recording.aborts.Load())
		}
	})

	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		input := newBlockingStdin()
		recording := newDictateRecording()
		result := make(chan error, 1)
		go func() {
			result <- (App{Stdin: input}).waitForDictateStop(ctx, recording)
		}()
		waitChannel(t, input.started, "stdin read")
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("context cancellation did not stop dictate wait")
		}
		if recording.aborts.Load() != 0 {
			t.Fatalf("Abort calls=%d", recording.aborts.Load())
		}
		waitChannel(t, input.cancelled, "stdin cancellation")
	})
}

type deadlineSink struct {
	deadline time.Time
}

func (s *deadlineSink) Deliver(ctx context.Context, _ string) error {
	s.deadline, _ = ctx.Deadline()
	return nil
}

func TestDictateDeliveryUsesBoundedContext(t *testing.T) {
	target := &deadlineSink{}
	before := time.Now()
	if err := deliverDictate(target, "final"); err != nil {
		t.Fatal(err)
	}
	if target.deadline.IsZero() {
		t.Fatal("livraison dictate sans deadline")
	}
	remaining := target.deadline.Sub(before)
	if remaining < sink.ExternalDeliveryTimeout-time.Second || remaining > sink.ExternalDeliveryTimeout+time.Second {
		t.Fatalf("budget de livraison = %s", remaining)
	}
}

func TestStatusFallsBackToPersistedResult(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	runtimeDir, err := xdg.RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	want := session.LastResult{
		Version: 1, SessionID: "previous", Result: session.Cancelled,
		StartedAt: now.Add(-time.Second), FinishedAt: now,
	}
	session.AddJournalWarning(&want)
	if err := session.WriteLastResult(runtimeDir, want); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"status", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var got statusOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Active || got.State != "idle" || got.LastResult == nil || got.LastResult.Result != session.Cancelled ||
		!hasJournalWarning(got.LastResult.Warnings) {
		t.Fatalf("status=%#v", got)
	}
	stdout.Reset()
	if code := Run(context.Background(), []string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "journal dégradé") {
		t.Fatalf("human status=%q", stdout.String())
	}
}

func TestStatusNeverPresentsOlderResultAfterPersistenceFailure(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	runtimeDir, err := xdg.RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := session.LastResult{
		Version: 1, SessionID: "previous-session", Result: session.Completed,
		StartedAt: now.Add(-time.Second), FinishedAt: now,
	}
	if err := session.WriteLastResult(runtimeDir, old); err != nil {
		t.Fatal(err)
	}
	if err := session.MarkLastResultPending(runtimeDir, "new-session"); err != nil {
		t.Fatal(err)
	}

	server, err := ipc.Listen(socketPath(runtimeDir), ipc.HandlerFunc(func(context.Context, ipc.Request) ipc.Response {
		return ipc.Success(string(session.Recording))
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServer := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx) }()

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"status", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("active code=%d stderr=%q", code, stderr.String())
	}
	var active statusOutput
	if err := json.Unmarshal(stdout.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if !active.Active || active.State != string(session.Recording) || active.LastResult != nil || active.LastResultStale {
		t.Fatalf("active status=%#v", active)
	}

	stopServer()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"status", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("inactive code=%d stderr=%q", code, stderr.String())
	}
	var inactive statusOutput
	if err := json.Unmarshal(stdout.Bytes(), &inactive); err != nil {
		t.Fatal(err)
	}
	if inactive.Active || inactive.State != string(session.Idle) || inactive.LastResult != nil || !inactive.LastResultStale {
		t.Fatalf("inactive status=%#v", inactive)
	}
	if strings.Contains(stdout.String(), old.SessionID) {
		t.Fatalf("stale session leaked through status: %s", stdout.String())
	}

	stdout.Reset()
	if code := Run(context.Background(), []string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("human code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "persistance incomplète") || strings.Contains(stdout.String(), old.SessionID) {
		t.Fatalf("human status=%q", stdout.String())
	}
	assertNoCLIResultTemporaryFiles(t, runtimeDir)
}

func assertNoCLIResultTemporaryFiles(t *testing.T, runtimeDir string) {
	t.Helper()
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".last-result") {
			t.Fatalf("partial result file left behind: %s", entry.Name())
		}
	}
}

func TestPersistJournalWarningExcludesFilesystemDetails(t *testing.T) {
	const canary = "MURMUR-SECRET-PATH-CANARY"
	runtimeDir := t.TempDir()
	now := time.Now().UTC()
	result := session.LastResult{
		Version: 1, SessionID: "session-journal", Result: session.Completed,
		StartedAt: now.Add(-time.Second), FinishedAt: now,
	}
	journalErr := &events.JournalError{
		Operation: events.JournalWrite,
		Err:       &os.PathError{Op: "write", Path: filepath.Join(runtimeDir, canary), Err: errors.New("refus")},
	}
	if err := persistJournalWarning(runtimeDir, &result, journalErr); err != nil {
		t.Fatal(err)
	}
	persisted, err := session.ReadLastResult(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(canary)) || len(persisted.Warnings) != 1 ||
		persisted.Warnings[0].Component != "session-journal" {
		t.Fatalf("persisted result=%s", encoded)
	}
}

func TestCommitLastResultRequiresEveryTerminalWrite(t *testing.T) {
	for _, test := range []struct {
		name       string
		persistErr error
	}{
		{
			name: "initial directory fsync",
			persistErr: &os.PathError{
				Op: "sync", Path: "/runtime", Err: syscall.EIO,
			},
		},
		{
			name: "journal warning rewrite ENOSPC",
			persistErr: &os.PathError{
				Op: "write", Path: "/runtime/last-result.json", Err: syscall.ENOSPC,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			result := session.LastResult{
				Version: 1, SessionID: "uncommitted-session", Result: session.Completed,
				StartedAt: time.Now().Add(-time.Second), FinishedAt: time.Now(),
			}
			if err := session.MarkLastResultPending(runtimeDir, result.SessionID); err != nil {
				t.Fatal(err)
			}
			// A directory fsync may fail after Rename made the complete result visible.
			if err := session.WriteLastResult(runtimeDir, result); err != nil {
				t.Fatal(err)
			}
			if err := commitLastResult(runtimeDir, result.SessionID, test.persistErr); err != nil {
				t.Fatalf("commit gate returned an additional error: %v", err)
			}
			if _, err := session.ReadLastResult(runtimeDir); !errors.Is(err, session.ErrLastResultStale) {
				t.Fatalf("ReadLastResult() error = %v, want stale", err)
			}
		})
	}
}

func TestInternalArgumentsAreStrict(t *testing.T) {
	lock, ready, configFD, err := parseInternalArgs([]string{"--lock-fd=3", "--ready-fd=4", "--config-fd=5"})
	if err != nil || lock != 3 || ready != 4 || configFD != 5 {
		t.Fatalf("parse=%d,%d,%d error=%v", lock, ready, configFD, err)
	}
	for _, args := range [][]string{
		{"--lock-fd=3"},
		{"--lock-fd=3", "--ready-fd=3", "--config-fd=5"},
		{"--lock-fd=3", "--ready-fd=4", "--config-fd=5", "--unknown"},
	} {
		if _, _, _, err := parseInternalArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestSessionSnapshotCarriesEffectiveConfigWithoutResolvedSecret(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sink.Mode = "stdout"
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.APIKeyEnv = "MURMUR_TEST_SECRET"
	cfg.STT.Providers[cfg.STT.Provider] = provider
	t.Setenv("MURMUR_TEST_SECRET", "must-not-cross-the-fd")

	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(sessionSnapshot{Config: cfg, ClipboardOnly: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), "must-not-cross-the-fd") {
		t.Fatalf("resolved secret in snapshot: %s", encoded.String())
	}
	var decoded sessionSnapshot
	if err := decodeSessionSnapshot(&encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Config.Sink.Mode != "stdout" || !decoded.ClipboardOnly || decoded.Config.STT.Providers[cfg.STT.Provider].APIKeyEnv != "MURMUR_TEST_SECRET" {
		t.Fatalf("snapshot = %#v", decoded)
	}
}

func TestSessionSnapshotIsBoundedAndErrorsAreClassified(t *testing.T) {
	var snapshot sessionSnapshot
	err := decodeSessionSnapshot(strings.NewReader(strings.Repeat("x", maxSessionSnapshotBytes+1)), &snapshot)
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "volumineuse") {
		t.Fatalf("oversized snapshot error = %v", err)
	}
	err = decodeSessionSnapshot(strings.NewReader("{"), &snapshot)
	if errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("invalid snapshot error = %v", err)
	}
}

func TestSessionDescriptorsMustBeTypedAndDistinct(t *testing.T) {
	dir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	configReader, configWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer configReader.Close()
	defer configWriter.Close()

	if err := validateSessionFDs(int(lock.Fd()), int(readyWriter.Fd()), int(configReader.Fd())); err != nil {
		t.Fatalf("valid descriptors rejected: %v", err)
	}
	alias, err := syscall.Dup(int(readyWriter.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(alias)
	if err := validateSessionFDs(int(lock.Fd()), int(readyWriter.Fd()), alias); errs.Classify(err) != errs.Internal {
		t.Fatalf("aliased pipes error = %v", err)
	}
	regular, err := os.CreateTemp(dir, "config-not-pipe-")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if err := validateSessionFDs(int(lock.Fd()), int(readyWriter.Fd()), int(regular.Fd())); errs.Classify(err) != errs.Internal {
		t.Fatalf("regular config descriptor error = %v", err)
	}
}

func TestSocketPathIsRuntimeLocal(t *testing.T) {
	dir := t.TempDir()
	if got, want := socketPath(dir), filepath.Join(dir, "session.sock"); got != want {
		t.Fatalf("socketPath=%q want=%q", got, want)
	}
}

func TestIPCCommandsResolveTheSameRuntimePath(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	runtimeDir, err := xdg.RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []ipc.Command
	server, err := ipc.Listen(socketPath(runtimeDir), ipc.HandlerFunc(func(_ context.Context, request ipc.Request) ipc.Response {
		mu.Lock()
		commands = append(commands, request.Command)
		mu.Unlock()
		return ipc.Success(string(session.Recording))
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()

	for _, args := range [][]string{{"stop"}, {"cancel"}, {"toggle"}, {"status"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("Run(%v)=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []ipc.Command{ipc.Stop, ipc.Cancel, ipc.Toggle, ipc.Status}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands=%v, want %v", commands, want)
	}
}

type blockingStdin struct {
	started   chan struct{}
	cancelled chan struct{}
	startOnce sync.Once
	cancel    sync.Once
}

func newBlockingStdin() *blockingStdin {
	return &blockingStdin{started: make(chan struct{}), cancelled: make(chan struct{})}
}

func (r *blockingStdin) Read([]byte) (int, error) {
	r.startOnce.Do(func() { close(r.started) })
	<-r.cancelled
	return 0, io.EOF
}

func (r *blockingStdin) CancelRead() error {
	r.cancel.Do(func() { close(r.cancelled) })
	return nil
}

type dictateRecording struct {
	mu     sync.Mutex
	done   chan struct{}
	err    error
	aborts atomic.Int32
}

func newDictateRecording() *dictateRecording {
	return &dictateRecording{done: make(chan struct{})}
}

func (r *dictateRecording) Ready() <-chan error {
	ready := make(chan error)
	close(ready)
	return ready
}

func (r *dictateRecording) Done() <-chan struct{} { return r.done }

func (r *dictateRecording) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *dictateRecording) Stop(context.Context) (audio.WavFile, error) {
	return audio.WavFile{}, errors.New("unexpected Stop")
}

func (r *dictateRecording) Abort() error {
	r.aborts.Add(1)
	return nil
}

func (r *dictateRecording) fail(err error) {
	r.mu.Lock()
	r.err = err
	close(r.done)
	r.mu.Unlock()
}

func waitChannel(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for %s", name)
	}
}

func writeCLITestExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

type cliProvider struct {
	name       string
	transcribe func(context.Context, stt.Request) (stt.Result, error)
}

func (p cliProvider) Name() string { return p.name }
func (p cliProvider) Transcribe(ctx context.Context, request stt.Request) (stt.Result, error) {
	return p.transcribe(ctx, request)
}

func cliWAV(t *testing.T, duration time.Duration) string {
	t.Helper()
	dataSize := uint32(int64(32000) * int64(duration) / int64(time.Second))
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], dataSize+36)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], 16000)
	binary.LittleEndian.PutUint32(header[28:32], 32000)
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataSize)
	path := filepath.Join(t.TempDir(), "mixed.wav")
	if err := os.WriteFile(path, append(header, make([]byte, dataSize)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var _ audio.Recording = (*dictateRecording)(nil)
var _ stdinReadCanceler = (*blockingStdin)(nil)
