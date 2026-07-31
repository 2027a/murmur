package acceptance

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/core"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/ipc"
	"git.2027a.net/mathieu/murmur/internal/session"
	"git.2027a.net/mathieu/murmur/internal/sink"
	"git.2027a.net/mathieu/murmur/internal/stt"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
	"git.2027a.net/mathieu/murmur/internal/ui"
)

func TestAcceptance_01_LANNominalCleansAndPastesOnce(t *testing.T) {
	var sttCalls, cleanupCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/transcriptions":
			sttCalls.Add(1)
			writeJSON(t, w, map[string]string{"text": "bonjour brut"})
		case "/v1/chat/completions":
			cleanupCalls.Add(1)
			writeJSON(t, w, cleanupResponse("bonjour propre"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lanURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	cfg := baseConfig(lanURL)
	provider := cfg.STT.Providers["local"]
	provider.Zone = "lan"
	cfg.STT.Providers["local"] = provider
	cfg.Policy.STTZones = []string{"lan"}
	cfg.Cleanup.Enabled = true
	cfg.Cleanup.Provider = "lan-cleanup"
	cfg.Cleanup.Providers["lan-cleanup"] = cleanupProviderConfig(lanURL, "lan", "cleanup")
	cfg.Policy.CleanupZones = []string{"lan"}
	cfg.Sink.Mode = "paste"
	cfg.Sink.ClipboardMode = "clean-only"

	executor := &sysexec.FakeExecutor{}
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), executor, nil, nil, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || result.Provider != "local" {
		t.Fatalf("result=%#v error=%v", result, runErr)
	}
	if sttCalls.Load() != 1 || cleanupCalls.Load() != 1 {
		t.Fatalf("calls: stt=%d cleanup=%d", sttCalls.Load(), cleanupCalls.Load())
	}
	calls := executor.Calls()
	if len(calls) != 2 || calls[0].Name != "wl-copy" || string(calls[0].Stdin) != "bonjour propre" || calls[1].Name != "wtype" {
		t.Fatalf("delivery calls=%#v", calls)
	}
}

func TestAcceptance_02_LocalOnlyNeverContactsInternetProvider(t *testing.T) {
	var localCalls, internetCalls atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localCalls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
			writeJSON(t, w, map[string]string{"text": "texte local"})
			return
		}
		writeJSON(t, w, cleanupResponse("texte local nettoye"))
	}))
	defer local.Close()
	internet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internetCalls.Add(1)
		http.Error(w, "unused", http.StatusInternalServerError)
	}))
	defer internet.Close()

	cfg := baseConfig(local.URL)
	cfg.Cleanup.Enabled = true
	cfg.Cleanup.Provider = "local-cleanup"
	cfg.Cleanup.Providers["local-cleanup"] = cleanupProviderConfig(local.URL, "local", "cleanup")
	cfg.STT.Providers["unused-cloud"] = providerConfig(internet.URL, "internet", "cloud", "fr")
	cloud := cfg.STT.Providers["unused-cloud"]
	cloud.AllowInsecure = true
	cfg.STT.Providers["unused-cloud"] = cloud

	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), &sysexec.FakeExecutor{}, &output, nil, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || output.String() != "texte local nettoye\n" {
		t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
	}
	if localCalls.Load() != 2 || internetCalls.Load() != 0 {
		t.Fatalf("requests: local=%d internet=%d", localCalls.Load(), internetCalls.Load())
	}
}

func TestAcceptance_03_CloudReceivesCredentialsAndIsEffectiveProvider(t *testing.T) {
	const secret = "acceptance-cloud-secret"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization=%q", got)
		}
		writeJSON(t, w, map[string]string{"text": "cloud transcript"})
	}))
	defer server.Close()
	t.Setenv("MURMUR_ACCEPTANCE_CLOUD_KEY", secret)

	cfg := baseConfig(server.URL)
	delete(cfg.STT.Providers, "local")
	cfg.STT.Provider = "cloud"
	cloud := providerConfig(server.URL, "internet", "cloud-model", "fr")
	cloud.AllowInsecure = true
	cloud.APIKeyEnv = "MURMUR_ACCEPTANCE_CLOUD_KEY"
	cfg.STT.Providers["cloud"] = cloud
	cfg.Policy.STTZones = []string{"internet"}

	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), &sysexec.FakeExecutor{}, &output, nil, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || result.Provider != "cloud" || calls.Load() != 1 || output.String() != "cloud transcript\n" {
		t.Fatalf("result=%#v error=%v calls=%d output=%q", result, runErr, calls.Load(), output.String())
	}
}

func TestAcceptance_04_PolicyBlocksFallbackBeforeAnyByte(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("blocked fallback reached its HTTP handler")
	}))
	listener := &countingListener{Listener: server.Listener}
	server.Listener = listener
	server.Start()
	defer server.Close()

	cfg := baseConfig(server.URL)
	cfg.STT.Fallback = "forbidden-cloud"
	cloud := providerConfig(server.URL, "internet", "cloud", "fr")
	cloud.AllowInsecure = true
	cfg.STT.Providers["forbidden-cloud"] = cloud
	cfg.Policy.STTZones = []string{"local"}

	_, err := core.BuildTranscriber(context.Background(), cfg, core.BuildOptions{})
	if errs.Classify(err) != errs.PolicyBlocked {
		t.Fatalf("BuildTranscriber() error=%v class=%s", err, errs.Classify(err))
	}
	if listener.accepted.Load() != 0 {
		t.Fatalf("accepted connections=%d", listener.accepted.Load())
	}
}

func TestAcceptance_05_FallbackReusesCaptureAudio(t *testing.T) {
	var mu sync.Mutex
	var primaryAudio, fallbackAudio []byte
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := multipartAudio(t, r)
		mu.Lock()
		primaryAudio = append([]byte(nil), data...)
		mu.Unlock()
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := multipartAudio(t, r)
		mu.Lock()
		fallbackAudio = append([]byte(nil), data...)
		mu.Unlock()
		writeJSON(t, w, map[string]string{"text": "fallback ok"})
	}))
	defer fallback.Close()

	cfg := baseConfig(primary.URL)
	cfg.STT.Fallback = "fallback"
	cfg.STT.Providers["fallback"] = providerConfig(fallback.URL, "local", "fallback", "fr")
	recorder := &countingRecorder{delegate: &audio.FakeRecorder{FixturePath: buildFixture(t, time.Second)}}
	var output bytes.Buffer
	o := buildWithRecorder(t, cfg, recorder, &sysexec.FakeExecutor{}, &output, nil, false)
	result, runErr := stopSession(t, o)
	mu.Lock()
	sameAudio := bytes.Equal(primaryAudio, fallbackAudio) && len(primaryAudio) > 44
	mu.Unlock()
	if runErr != nil || result.Result != session.Completed || result.Provider != "fallback" || recorder.starts.Load() != 1 || !sameAudio {
		t.Fatalf("result=%#v error=%v starts=%d same_audio=%v", result, runErr, recorder.starts.Load(), sameAudio)
	}
	if len(result.Warnings) != 1 || output.String() != "fallback ok\n" {
		t.Fatalf("warnings=%v output=%q", result.Warnings, output.String())
	}
}

func TestConfiguredRetriesAreAppliedByCore(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		writeJSON(t, w, map[string]string{"text": "retry ok"})
	}))
	defer server.Close()
	cfg := baseConfig(server.URL)
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.Retries = 1
	provider.RetryBackoffMS = 1
	cfg.STT.Providers[cfg.STT.Provider] = provider
	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), &sysexec.FakeExecutor{}, &output, nil, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || calls.Load() != 2 || output.String() != "retry ok\n" {
		t.Fatalf("result=%#v error=%v calls=%d output=%q", result, runErr, calls.Load(), output.String())
	}
}

func TestAcceptance_06_CleanupFailureDeliversDeterministicFallbackAndDegradedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
			writeJSON(t, w, map[string]string{"text": "ouvrez les guillemets brut fermez les guillemets"})
			return
		}
		http.Error(w, "cleanup unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := baseConfig(server.URL)
	cfg.Cleanup.Enabled = true
	cfg.Cleanup.Provider = "cleanup"
	cfg.Cleanup.Providers["cleanup"] = cleanupProviderConfig(server.URL, "local", "cleanup")
	bus := events.NewBus()
	stream, unsubscribe := bus.Subscribe(64)
	defer unsubscribe()
	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), &sysexec.FakeExecutor{}, &output, bus, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || output.String() != "« brut »\n" || len(result.Warnings) != 1 {
		t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
	}
	if !hasEvent(stream, events.Degraded, "cleanup") {
		t.Fatal("cleanup degradation event missing")
	}
}

func TestAcceptance_07_LiveSnapshotsReconcatenateWithoutSampleLoss(t *testing.T) {
	var mu sync.Mutex
	audioByIndex := make(map[int][]byte)
	first := make(chan struct{})
	var firstOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := multipartAudio(t, r)
		index := sampleSegmentIndex(t, data)
		mu.Lock()
		audioByIndex[index] = append([]byte(nil), data[44:]...)
		mu.Unlock()
		firstOnce.Do(func() { close(first) })
		writeJSON(t, w, map[string]string{"text": fmt.Sprintf("segment-%d", index)})
	}))
	defer server.Close()

	fixture := buildFixture(t, 2500*time.Millisecond)
	cfg := baseConfig(server.URL)
	cfg.STT.ChunkSeconds = 1
	cfg.STT.LiveChunking = true
	cfg.STT.ChunkWorkers = 2
	bus := events.NewBus()
	stream, unsubscribe := bus.Subscribe(64)
	defer unsubscribe()
	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, fixture, &sysexec.FakeExecutor{}, &output, bus, false)
	done := runSession(o)
	waitForState(t, o, session.Recording)
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("aucun segment transcrit pendant la capture")
	}
	if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
		t.Fatalf("stop response=%#v", response)
	}
	result, runErr := awaitSession(t, done)
	if runErr != nil || result.Result != session.Completed || output.String() != "segment-0 segment-1\n" {
		t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
	}

	mu.Lock()
	joined := append(append([]byte(nil), audioByIndex[0]...), audioByIndex[1]...)
	lengths := []int{len(audioByIndex[0]), len(audioByIndex[1])}
	mu.Unlock()
	original, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(joined, original[44:]) || lengths[0] != wav.ByteRate || lengths[1] != wav.ByteRate*3/2 {
		t.Fatalf("reconcaténation live incorrecte: tailles=%v joint=%d original=%d", lengths, len(joined), len(original)-44)
	}
	if !hasEvent(stream, events.PartialStarted, "local") || !hasEvent(stream, events.PartialDone, "local") {
		t.Fatal("événements partial_started/partial_done manquants")
	}
}

func TestAcceptance_08_LiveWorkersAreBoundedOrderedAndCancellable(t *testing.T) {
	t.Run("inverse completion remains ordered and bounded", func(t *testing.T) {
		var active, maximum atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data := multipartAudio(t, r)
			index := sampleSegmentIndex(t, data)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				seen := maximum.Load()
				if current <= seen || maximum.CompareAndSwap(seen, current) {
					break
				}
			}
			time.Sleep(time.Duration(4-index) * 20 * time.Millisecond)
			writeJSON(t, w, map[string]string{"text": fmt.Sprintf("s%d", index)})
		}))
		defer server.Close()
		cfg := baseConfig(server.URL)
		cfg.STT.ChunkSeconds = 1
		cfg.STT.LiveChunking = true
		cfg.STT.ChunkWorkers = 2
		var output bytes.Buffer
		o := buildOrchestrator(t, cfg, buildFixture(t, 4500*time.Millisecond), &sysexec.FakeExecutor{}, &output, nil, false)
		result, runErr := stopSession(t, o)
		if runErr != nil || result.Result != session.Completed || output.String() != "s0 s1 s2 s3\n" {
			t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
		}
		if maximum.Load() != 2 {
			t.Fatalf("concurrence maximale=%d, attendu 2", maximum.Load())
		}
	})

	t.Run("segment failure never delivers by default", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data := multipartAudio(t, r)
			index := sampleSegmentIndex(t, data)
			if index == 1 {
				http.Error(w, "failed", http.StatusServiceUnavailable)
				return
			}
			writeJSON(t, w, map[string]string{"text": fmt.Sprintf("s%d", index)})
		}))
		defer server.Close()
		cfg := baseConfig(server.URL)
		cfg.STT.ChunkSeconds = 1
		cfg.STT.LiveChunking = true
		bus := events.NewBus()
		stream, unsubscribe := bus.Subscribe(32)
		defer unsubscribe()
		var output bytes.Buffer
		o := buildOrchestrator(t, cfg, buildFixture(t, 2500*time.Millisecond), &sysexec.FakeExecutor{}, &output, bus, false)
		result, runErr := stopSession(t, o)
		if runErr == nil || result.Result != session.Failed || output.Len() != 0 {
			t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
		}
		if !hasEvent(stream, events.PartialFailed, "local") {
			t.Fatal("événement partial_failed manquant")
		}
	})

	t.Run("partial recovery is an explicit warning", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data := multipartAudio(t, r)
			index := sampleSegmentIndex(t, data)
			if index == 1 {
				http.Error(w, "failed", http.StatusServiceUnavailable)
				return
			}
			writeJSON(t, w, map[string]string{"text": fmt.Sprintf("s%d", index)})
		}))
		defer server.Close()
		cfg := baseConfig(server.URL)
		cfg.STT.ChunkSeconds = 1
		cfg.STT.LiveChunking = true
		cfg.STT.AllowPartial = true
		var output bytes.Buffer
		o := buildOrchestrator(t, cfg, buildFixture(t, 2500*time.Millisecond), &sysexec.FakeExecutor{}, &output, nil, false)
		result, runErr := stopSession(t, o)
		if runErr != nil || result.Result != session.Completed || output.String() != "s0\n" || len(result.Warnings) != 1 || result.Warnings[0] != stt.WarningPartial {
			t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
		}
	})

	t.Run("provider mixing is opt-in and traced", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data := multipartAudio(t, r)
			index := sampleSegmentIndex(t, data)
			if strings.HasPrefix(r.URL.Path, "/primary/") && index == 1 {
				http.Error(w, "failed", http.StatusServiceUnavailable)
				return
			}
			provider := "primary"
			if strings.HasPrefix(r.URL.Path, "/fallback/") {
				provider = "fallback"
			}
			writeJSON(t, w, map[string]string{"text": fmt.Sprintf("%s-%d", provider, index)})
		}))
		defer server.Close()
		cfg := baseConfig(server.URL + "/primary")
		cfg.STT.ChunkSeconds = 1
		cfg.STT.LiveChunking = true
		cfg.STT.AllowMixedProviders = true
		cfg.STT.Fallback = "fallback"
		cfg.STT.Providers["fallback"] = providerConfig(server.URL+"/fallback", "local", "fallback", "fr")
		var output bytes.Buffer
		o := buildOrchestrator(t, cfg, buildFixture(t, 2500*time.Millisecond), &sysexec.FakeExecutor{}, &output, nil, false)
		result, runErr := stopSession(t, o)
		if runErr != nil || result.Result != session.Completed || output.String() != "primary-0 fallback-1\n" {
			t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
		}
		if !containsString(result.Warnings, stt.WarningMixedProviders) {
			t.Fatalf("warning de mélange absent: %v", result.Warnings)
		}
	})

	t.Run("cancel drains workers", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		var active atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			active.Add(1)
			defer active.Add(-1)
			once.Do(func() { close(started) })
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer server.Close()
		cfg := baseConfig(server.URL)
		cfg.STT.ChunkSeconds = 1
		cfg.STT.LiveChunking = true
		cfg.STT.ChunkWorkers = 2
		var output bytes.Buffer
		o := buildOrchestrator(t, cfg, buildFixture(t, 3500*time.Millisecond), &sysexec.FakeExecutor{}, &output, nil, false)
		done := runSession(o)
		waitForState(t, o, session.Recording)
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("worker STT non démarré")
		}
		if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel)); !response.OK {
			t.Fatalf("cancel response=%#v", response)
		}
		result, runErr := awaitSession(t, done)
		if runErr != nil || result.Result != session.Cancelled || output.Len() != 0 {
			t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
		}
		close(release)
		deadline := time.Now().Add(time.Second)
		for active.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if active.Load() != 0 {
			t.Fatalf("workers HTTP encore actifs: %d", active.Load())
		}
	})
}

func TestAcceptance_10_ClipboardOnlyOverridesGlobalPaste(t *testing.T) {
	server := transcriptionServer(t, "clipboard text")
	defer server.Close()
	cfg := baseConfig(server.URL)
	cfg.Sink.Mode = "paste"
	cfg.Sink.ClipboardMode = "clean-only"
	executor := &sysexec.FakeExecutor{}
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), executor, nil, nil, true)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed {
		t.Fatalf("result=%#v error=%v", result, runErr)
	}
	calls := executor.Calls()
	if len(calls) != 1 || calls[0].Name != "wl-copy" || string(calls[0].Stdin) != "clipboard text" {
		t.Fatalf("clipboard-only calls=%#v", calls)
	}
}

func TestAcceptance_11_EmptyTranscriptProducesNoSpeechWithoutSink(t *testing.T) {
	server := transcriptionServer(t, "   ")
	defer server.Close()
	cfg := baseConfig(server.URL)
	cfg.Sink.Mode = "file"
	cfg.Sink.File = filepath.Join(t.TempDir(), "must-not-exist.txt")
	executor := &sysexec.FakeExecutor{}
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), executor, nil, nil, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.NoSpeech || result.Error != nil {
		t.Fatalf("result=%#v error=%v", result, runErr)
	}
	if _, err := os.Stat(cfg.Sink.File); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final file exists or stat failed unexpectedly: %v", err)
	}
	if len(executor.Calls()) != 0 {
		t.Fatalf("external sink calls=%#v", executor.Calls())
	}
}

func TestAcceptance_12_NoUIOrDesktopToolsRequiredForStdout(t *testing.T) {
	server := transcriptionServer(t, "stdout only")
	defer server.Close()
	cfg := baseConfig(server.URL)
	executor := &sysexec.FakeExecutor{RunFunc: func(context.Context, string, []string, []byte) ([]byte, error) {
		return nil, errors.New("desktop command must not run")
	}}
	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), executor, &output, nil, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || output.String() != "stdout only\n" || len(executor.Calls()) != 0 {
		t.Fatalf("result=%#v error=%v output=%q calls=%#v", result, runErr, output.String(), executor.Calls())
	}

	t.Run("composable subprocesses", func(t *testing.T) {
		var sttCalls, cleanupCalls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/audio/transcriptions":
				sttCalls.Add(1)
				writeJSON(t, w, map[string]string{"text": "bonjour brut"})
			case "/v1/chat/completions":
				cleanupCalls.Add(1)
				writeJSON(t, w, cleanupResponse("bonjour propre"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()

		repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(t.TempDir(), "murmur")
		build := exec.Command("go", "build", "-o", binary, "./cmd/murmur")
		build.Dir = repoRoot
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build binary: %v\n%s", err, output)
		}
		base := t.TempDir()
		configDir := filepath.Join(base, "config", "murmur")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		dictatedFile := filepath.Join(base, "dictated.txt")
		writeComposableConfig(t, filepath.Join(configDir, "config.toml"), server.URL, dictatedFile)
		env := replaceEnvironment(os.Environ(),
			"PATH=",
			"XDG_CONFIG_HOME="+filepath.Join(base, "config"),
			"XDG_RUNTIME_DIR="+filepath.Join(base, "runtime"),
		)
		fixture := buildFixture(t, 2200*time.Millisecond)

		stdout, stderr, code := runSubprocess(t, env, nil, binary, "transcribe", fixture, "--json", "--events")
		if code != 0 {
			t.Fatalf("transcribe code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		var structured struct {
			Version         int    `json:"v"`
			Command         string `json:"command"`
			Success         bool   `json:"success"`
			Provider        string `json:"provider"`
			Model           string `json:"model"`
			Language        string `json:"language"`
			TranscriptRaw   string `json:"transcript_raw"`
			TranscriptFinal string `json:"transcript_final"`
			SegmentCount    int    `json:"segment_count"`
			Segments        []any  `json:"segments"`
			Warnings        []any  `json:"warnings"`
			Error           any    `json:"error"`
		}
		if err := json.Unmarshal(stdout, &structured); err != nil {
			t.Fatalf("structured output: %v: %s", err, stdout)
		}
		if structured.Version != 1 || structured.Command != "transcribe" || !structured.Success || structured.Provider != "local" || structured.Model != "stt-model" || structured.Language != "fr" || structured.TranscriptRaw != "bonjour brut bonjour brut" || structured.TranscriptFinal != "bonjour propre" || structured.SegmentCount != 2 || len(structured.Segments) != 2 || structured.Error != nil {
			t.Fatalf("structured=%#v", structured)
		}
		assertJSONLEvents(t, stderr)
		if bytes.Contains(stderr, []byte("bonjour")) {
			t.Fatalf("transcript leaked into events: %s", stderr)
		}

		stdout, stderr, code = runSubprocess(t, env, strings.NewReader("entrée brute"), binary, "cleanup", "--json", "--events")
		if code != 0 || !bytes.Contains(stdout, []byte(`"transcript_raw":"entrée brute"`)) || !bytes.Contains(stdout, []byte(`"transcript_final":"bonjour propre"`)) {
			t.Fatalf("cleanup code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertJSONLEvents(t, stderr)

		tools := t.TempDir()
		writeExecutable(t, filepath.Join(tools, "pw-record"), `#!/bin/sh
for output do :; done
cp "$MURMUR_TEST_WAV" "$output" || exit 1
trap 'exit 130' INT TERM
while :; do sleep 1; done
`)
		dictateFixture := buildFixture(t, time.Second)
		dictateEnv := replaceEnvironment(env,
			"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
			"MURMUR_TEST_WAV="+dictateFixture,
		)
		stdout, stderr, code = runSubprocess(t, dictateEnv, strings.NewReader(""), binary, "dictate", "--sink", "stdout")
		if code != 0 || string(stdout) != "bonjour propre\n" || len(stderr) != 0 {
			t.Fatalf("dictate stdout code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		stdout, stderr, code = runSubprocess(t, dictateEnv, strings.NewReader(""), binary, "dictate", "--sink", "file", "--json", "--events")
		if code != 0 || !bytes.Contains(stdout, []byte(`"command":"dictate"`)) || bytes.Contains(stdout, []byte("bonjour propre\nbonjour propre")) {
			t.Fatalf("dictate file code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertJSONLEvents(t, stderr)
		data, err := os.ReadFile(dictatedFile)
		if err != nil || string(data) != "bonjour propre" {
			t.Fatalf("dictated file=%q error=%v", data, err)
		}

		invalid := filepath.Join(t.TempDir(), "invalid.wav")
		if err := os.WriteFile(invalid, []byte("not a wav"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code = runSubprocess(t, env, nil, binary, "transcribe", "--json", invalid)
		if code != 1 || len(stderr) != 0 || !bytes.Contains(stdout, []byte(`"class":"audio_failed"`)) || bytes.Contains(stdout, []byte("not a wav")) {
			t.Fatalf("invalid WAV code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if sttCalls.Load() != 4 || cleanupCalls.Load() != 4 {
			t.Fatalf("provider calls: stt=%d cleanup=%d", sttCalls.Load(), cleanupCalls.Load())
		}

		missingBase := t.TempDir()
		missingEnv := replaceEnvironment(os.Environ(), "PATH=", "XDG_CONFIG_HOME="+filepath.Join(missingBase, "config"))
		stdout, stderr, code = runSubprocess(t, missingEnv, strings.NewReader("inchangé\n"), binary, "cleanup")
		if code != 0 || string(stdout) != "inchangé\n" || len(stderr) != 0 {
			t.Fatalf("cleanup sans config code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})
}

func TestAcceptance_13_OSDFailureDoesNotStopTranscriptionOrSink(t *testing.T) {
	server := transcriptionServer(t, "delivered without osd")
	defer server.Close()
	cfg := baseConfig(server.URL)
	cfg.UI.Mode = "osd"
	cfg.UI.OSD.Helper = "custom-murmur-osd"
	cfg.UI.OSD.Position = "center"
	cfg.UI.OSD.SuccessTimeoutMS = 1234
	cfg.UI.OSD.ErrorTimeoutMS = 5678
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return nil, errors.New("OSD unavailable")
	}}
	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), executor, &output, nil, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || output.String() != "delivered without osd\n" {
		t.Fatalf("result=%#v error=%v output=%q", result, runErr, output.String())
	}
	calls := executor.Calls()
	wantArgs := []string{"--position", "center", "--success-timeout-ms", "1234", "--error-timeout-ms", "5678"}
	if len(calls) == 0 {
		t.Fatalf("OSD attempts=%#v", calls)
	}
	for _, call := range calls {
		if call.Name != "custom-murmur-osd" || !reflect.DeepEqual(call.Args, wantArgs) {
			t.Fatalf("OSD call=%#v", call)
		}
	}
}

func TestAcceptance_17_SegmentFailureRestartsFallbackWithoutFalseProvider(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/primary/"):
			call := primaryCalls.Add(1)
			_ = multipartAudio(t, r)
			if call == 2 {
				http.Error(w, "segment failed", http.StatusServiceUnavailable)
				return
			}
			writeJSON(t, w, map[string]string{"text": fmt.Sprintf("primary-%d", call)})
		case strings.HasPrefix(r.URL.Path, "/fallback/"):
			call := fallbackCalls.Add(1)
			_ = multipartAudio(t, r)
			writeJSON(t, w, map[string]string{"text": fmt.Sprintf("fallback-%d", call)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := baseConfig(server.URL + "/primary")
	cfg.STT.ChunkSeconds = 1
	cfg.STT.Fallback = "fallback"
	cfg.STT.Providers["fallback"] = providerConfig(server.URL+"/fallback", "local", "fallback", "fr")
	bus := events.NewBus()
	stream, unsubscribe := bus.Subscribe(64)
	defer unsubscribe()
	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, 3*time.Second), &sysexec.FakeExecutor{}, &output, bus, false)
	result, runErr := stopSession(t, o)
	if runErr != nil || result.Result != session.Completed || result.Provider != "fallback" || primaryCalls.Load() != 2 || fallbackCalls.Load() != 3 {
		t.Fatalf("result=%#v error=%v primary=%d fallback=%d", result, runErr, primaryCalls.Load(), fallbackCalls.Load())
	}
	if output.String() != "fallback-1 fallback-2 fallback-3\n" || len(result.Warnings) != 1 {
		t.Fatalf("output=%q warnings=%v", output.String(), result.Warnings)
	}
	if !hasEvent(stream, events.ProviderSelected, "fallback") {
		t.Fatal("effective fallback provider event missing")
	}
}

func TestAcceptance_18_EnglishCommandsMatchConfiguredTypography(t *testing.T) {
	server := transcriptionServer(t, "open quotation marks test close quotation marks new line second new paragraph third")
	defer server.Close()
	cfg := baseConfig(server.URL)
	cfg.General.CommandPack = "auto"
	provider := cfg.STT.Providers["local"]
	provider.Language = "en"
	cfg.STT.Providers["local"] = provider
	var output bytes.Buffer
	o := buildOrchestrator(t, cfg, buildFixture(t, time.Second), &sysexec.FakeExecutor{}, &output, nil, false)
	result, runErr := stopSession(t, o)
	want := "“test”\nsecond\n\nthird\n"
	if runErr != nil || result.Result != session.Completed || output.String() != want {
		t.Fatalf("result=%#v error=%v output=%q want=%q", result, runErr, output.String(), want)
	}
}

func TestBinaryProcessStartReexecSocketStopAndCancel(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	binaryPath := filepath.Join(tools, "murmur")
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd/murmur")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, output)
	}
	fixture := buildFixture(t, time.Second)
	writeExecutable(t, filepath.Join(tools, "pw-record"), `#!/bin/sh
for output do :; done
cp "$MURMUR_TEST_WAV" "$output" || exit 1
trap 'exit 130' INT TERM
while :; do sleep 1; done
`)

	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		writeJSON(t, w, map[string]string{"text": "binary process text"})
	}))
	defer server.Close()

	for _, command := range []string{"stop", "cancel"} {
		t.Run(command, func(t *testing.T) {
			before := providerCalls.Load()
			base := t.TempDir()
			configDir := filepath.Join(base, "config", "murmur")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			outputFile := filepath.Join(base, "delivered.txt")
			writeProcessConfig(t, filepath.Join(configDir, "config.toml"), server.URL, outputFile)
			env := append(os.Environ(),
				"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
				"XDG_CONFIG_HOME="+filepath.Join(base, "config"),
				"XDG_RUNTIME_DIR="+filepath.Join(base, "runtime"),
				"MURMUR_TEST_WAV="+fixture,
			)
			if err := os.MkdirAll(filepath.Join(base, "runtime"), 0o700); err != nil {
				t.Fatal(err)
			}
			runBinary(t, env, binaryPath, "start")
			runtimeDir := filepath.Join(base, "runtime", "murmur")
			t.Cleanup(func() {
				if _, err := os.Stat(filepath.Join(runtimeDir, "session.sock")); err == nil {
					cmd := exec.Command(binaryPath, "cancel")
					cmd.Env = env
					_ = cmd.Run()
				}
			})
			waitForActiveState(t, env, binaryPath)
			runBinary(t, env, binaryPath, command)
			result := waitForProcessResult(t, runtimeDir)
			if command == "stop" {
				if result.Result != session.Completed || providerCalls.Load() != before+1 {
					t.Fatalf("result=%#v provider calls before=%d after=%d", result, before, providerCalls.Load())
				}
				data, err := os.ReadFile(outputFile)
				if err != nil || string(data) != "binary process text" {
					t.Fatalf("delivered file=%q error=%v", data, err)
				}
			} else if result.Result != session.Cancelled || providerCalls.Load() != before {
				t.Fatalf("result=%#v provider calls before=%d after=%d", result, before, providerCalls.Load())
			}
		})
	}
}

func baseConfig(baseURL string) config.Config {
	cfg := config.Defaults()
	cfg.General.CommandPack = "fr"
	cfg.STT.Providers["local"] = providerConfig(baseURL, "local", "stt", "fr")
	cfg.Sink.Mode = "stdout"
	cfg.Sink.ClipboardMode = "none"
	cfg.UI.Mode = "none"
	cfg.UI.Sound = false
	return cfg
}

func providerConfig(baseURL, zone, model, language string) config.ProviderConfig {
	return config.ProviderConfig{
		Type: "openai-compatible", BaseURL: baseURL + "/v1", Model: model,
		Language: language, TimeoutS: 2, Zone: zone,
	}
}

func cleanupProviderConfig(baseURL, zone, model string) config.CleanupProviderConfig {
	return config.CleanupProviderConfig{
		Type: "openai-compatible", BaseURL: baseURL + "/v1", Model: model,
		TimeoutS: 2, Zone: zone,
	}
}

func buildOrchestrator(t *testing.T, cfg config.Config, fixture string, executor sysexec.Executor, stdout io.Writer, bus *events.Bus, clipboardOnly bool, interfaces ...ui.UI) *session.Orchestrator {
	t.Helper()
	return buildWithRecorder(t, cfg, &audio.FakeRecorder{FixturePath: fixture}, executor, stdout, bus, clipboardOnly, interfaces...)
}

func buildWithRecorder(t *testing.T, cfg config.Config, recorder audio.Recorder, executor sysexec.Executor, stdout io.Writer, bus *events.Bus, clipboardOnly bool, interfaces ...ui.UI) *session.Orchestrator {
	t.Helper()
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("invalid acceptance config: %v", err)
	}
	runtimeDir := t.TempDir()
	var userInterface ui.UI
	if len(interfaces) > 0 {
		userInterface = interfaces[0]
	}
	o, err := session.Build(session.BuildOptions{
		Context: context.Background(), Config: cfg, RuntimeDir: runtimeDir, SessionID: "acceptance",
		ClipboardOnly: clipboardOnly, Stdout: stdout, Executor: executor, Bus: bus, UI: userInterface, Recorder: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func stopSession(t *testing.T, o *session.Orchestrator) (session.LastResult, error) {
	t.Helper()
	done := runSession(o)
	waitForState(t, o, session.Recording)
	response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
	if !response.OK {
		t.Fatalf("stop response=%#v", response)
	}
	return awaitSession(t, done)
}

type sessionOutcome struct {
	result session.LastResult
	err    error
}

func runSession(o *session.Orchestrator) <-chan sessionOutcome {
	done := make(chan sessionOutcome, 1)
	go func() {
		result, err := o.Run()
		done <- sessionOutcome{result: result, err: err}
	}()
	return done
}

func awaitSession(t *testing.T, done <-chan sessionOutcome) (session.LastResult, error) {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome.result, outcome.err
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrator did not terminate")
		return session.LastResult{}, nil
	}
}

func waitForState(t *testing.T, o *session.Orchestrator, wanted session.State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response := o.Handle(context.Background(), ipc.NewRequest(ipc.Status))
		if response.OK && response.State == string(wanted) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session never reached %s", wanted)
}

func transcriptionServer(t *testing.T, transcript string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, map[string]string{"text": transcript})
	}))
}

func cleanupResponse(text string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": text}}}}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode HTTP response: %v", err)
	}
}

func multipartAudio(t *testing.T, r *http.Request) []byte {
	t.Helper()
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		t.Errorf("parse multipart: %v", err)
		return nil
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		t.Errorf("multipart audio: %v", err)
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Errorf("read multipart audio: %v", err)
	}
	return data
}

func buildFixture(t *testing.T, duration time.Duration) string {
	t.Helper()
	dataSize := uint32(int64(wav.ByteRate) * duration.Nanoseconds() / int64(time.Second))
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], dataSize+36)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], wav.Channels)
	binary.LittleEndian.PutUint32(header[24:28], wav.SampleRate)
	binary.LittleEndian.PutUint32(header[28:32], wav.ByteRate)
	binary.LittleEndian.PutUint16(header[32:34], wav.BlockAlign)
	binary.LittleEndian.PutUint16(header[34:36], wav.BitsPerSample)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataSize)
	payload := make([]byte, dataSize)
	for i := 0; i < len(payload)/2; i++ {
		binary.LittleEndian.PutUint16(payload[i*2:i*2+2], uint16(int16(i%32767)))
	}
	path := filepath.Join(t.TempDir(), "fixture.wav")
	if err := os.WriteFile(path, append(header, payload...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sampleSegmentIndex(t *testing.T, data []byte) int {
	t.Helper()
	if len(data) < 46 {
		t.Fatalf("segment WAV trop court: %d", len(data))
	}
	first := int(binary.LittleEndian.Uint16(data[44:46]))
	for index := 0; index < 32; index++ {
		if (index*wav.SampleRate)%32767 == first {
			return index
		}
	}
	t.Fatalf("premier échantillon de segment inconnu: %d", first)
	return -1
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func hasEvent(stream <-chan events.Event, kind events.Type, componentOrProvider string) bool {
	for len(stream) > 0 {
		event := <-stream
		if event.Type == kind && (event.Component == componentOrProvider || event.Provider == componentOrProvider) {
			return true
		}
	}
	return false
}

type providerFunc struct {
	name       string
	transcribe func(context.Context, stt.Request) (stt.Result, error)
}

func (p providerFunc) Name() string { return p.name }
func (p providerFunc) Transcribe(ctx context.Context, request stt.Request) (stt.Result, error) {
	return p.transcribe(ctx, request)
}

type recordingSink struct {
	mu    sync.Mutex
	calls []string
}

func (s *recordingSink) Deliver(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, text)
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type countingRecorder struct {
	delegate audio.Recorder
	starts   atomic.Int32
}

func (r *countingRecorder) Start(ctx context.Context, options audio.RecordOptions) (audio.Recording, error) {
	r.starts.Add(1)
	return r.delegate.Start(ctx, options)
}

type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeProcessConfig(t *testing.T, path, baseURL, output string) {
	t.Helper()
	content := fmt.Sprintf(`[general]
profile = "local"
command_pack = "fr"

[stt]
provider = "local"
fallback = ""
chunk_seconds = 0

[stt.providers.local]
type = "openai-compatible"
base_url = %q
model = "test"
language = "fr"
timeout_s = 2
zone = "local"

[cleanup]
enabled = false

[policy]
stt_zones = ["local"]
cleanup_zones = ["local"]

[sink]
mode = "file"
clipboard_mode = "none"
paste_shortcuts = ["ctrl+v"]
type_newlines = "keep"
file = %q

[ui]
mode = "none"
sound = false
`, baseURL+"/v1", output)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeComposableConfig(t *testing.T, path, baseURL, output string) {
	t.Helper()
	content := fmt.Sprintf(`[general]
command_pack = "fr"

[stt]
provider = "local"
chunk_seconds = 1
chunk_workers = 2

[stt.providers.local]
type = "openai-compatible"
base_url = %q
model = "stt-model"
language = "fr"
timeout_s = 2
zone = "local"

[cleanup]
enabled = true
provider = "clean"

[cleanup.providers.clean]
type = "openai-compatible"
base_url = %q
model = "cleanup-model"
timeout_s = 2
zone = "local"

[policy]
stt_zones = ["local"]
cleanup_zones = ["local"]

[sink]
mode = "stdout"
clipboard_mode = "none"
file = %q

[ui]
mode = "none"
sound = false
`, baseURL+"/v1", baseURL+"/v1", output)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runBinary(t *testing.T, env []string, binary string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", binary, args, err, output)
	}
	return output
}

func runSubprocess(t *testing.T, env []string, stdin io.Reader, binary string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %v timed out: %v", binary, args, ctx.Err())
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%s %v: %v", binary, args, err)
		}
		code = exit.ExitCode()
	}
	return stdout.Bytes(), stderr.Bytes(), code
}

func assertJSONLEvents(t *testing.T, data []byte) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("too few events: %q", data)
	}
	for index, line := range lines {
		var event events.Event
		if err := json.Unmarshal(line, &event); err != nil || event.Validate() != nil {
			t.Fatalf("event %d invalid: %v: %q", index, err, line)
		}
	}
}

func replaceEnvironment(base []string, replacements ...string) []string {
	names := make(map[string]struct{}, len(replacements))
	for _, replacement := range replacements {
		name, _, _ := strings.Cut(replacement, "=")
		names[name] = struct{}{}
	}
	result := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := names[name]; !replaced {
			result = append(result, entry)
		}
	}
	return append(result, replacements...)
}

func waitForActiveState(t *testing.T, env []string, binary string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		output := runBinary(t, env, binary, "status", "--json")
		var status struct {
			Active bool   `json:"active"`
			State  string `json:"state"`
		}
		if json.Unmarshal(output, &status) == nil && status.Active && (status.State == "starting" || status.State == "recording") {
			if status.State == "recording" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("binary session never reached recording")
}

func waitForProcessResult(t *testing.T, runtimeDir string) session.LastResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result, err := session.ReadLastResult(runtimeDir)
		_, socketErr := os.Stat(filepath.Join(runtimeDir, "session.sock"))
		if err == nil && errors.Is(socketErr, os.ErrNotExist) {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("binary session did not persist a terminal result")
	return session.LastResult{}
}

var (
	_ stt.Provider   = providerFunc{}
	_ sink.Sink      = (*recordingSink)(nil)
	_ audio.Recorder = (*countingRecorder)(nil)
)
