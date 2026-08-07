package core

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/stt"
)

func TestBuildProcessorUsesConfiguredASCIIQuotes(t *testing.T) {
	cfg := config.Defaults()
	cfg.General.CommandPack = "fr"
	cfg.General.ASCIIQuotes = true
	processor, err := BuildProcessor(context.Background(), cfg, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := processor.Run(context.Background(), "ouvre les guillemets salut ferme les guillemets")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `" salut "` {
		t.Fatalf("processed text = %q", result.Text)
	}
}

func TestBuildProcessorPreservesCleanupHTTP503Class(t *testing.T) {
	const canary = "MURMUR-SENSITIVE-HTTP-BODY-CANARY"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, canary, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cfg := config.Defaults()
	cfg.Cleanup.Enabled = true
	cfg.Cleanup.Provider = "cleanup"
	cfg.Cleanup.Providers["cleanup"] = config.CleanupProviderConfig{
		Type: "openai-compatible", BaseURL: server.URL, Model: "test", TimeoutS: 1, Zone: "local",
	}
	processor, err := BuildProcessor(context.Background(), cfg, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := processor.Run(context.Background(), "texte")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "texte" || len(result.Warnings) != 1 || result.Warnings[0].Class != errs.ServerError || result.Warnings[0].Component != "cleanup" {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.Warnings[0].Message, canary) {
		t.Fatalf("sensitive HTTP body leaked in warning: %#v", result.Warnings[0])
	}
}

func TestBuildCleanupPropagatesAllowInsecure(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cleanup.Enabled = true
	cfg.Cleanup.Provider = "cleanup"
	cfg.Cleanup.Providers["cleanup"] = config.CleanupProviderConfig{
		Type: "openai-compatible", BaseURL: "http://example.com/v1", Model: "test", TimeoutS: 1, Zone: "internet",
		AllowInsecure: true,
	}
	cfg.Policy.CleanupZones = []string{"internet"}
	if _, err := BuildCleanup(context.Background(), cfg, BuildOptions{}); err != nil {
		t.Fatalf("BuildCleanup() error = %v", err)
	}
}

func TestBuildTranscriberRejectsCrossOriginHealthBeforeProviderBuild(t *testing.T) {
	cfg := config.Defaults()
	provider := cfg.STT.Providers[cfg.STT.Provider]
	provider.HealthEnabled = true
	provider.HealthEndpoint = "http://127.0.0.1:9000/health"
	cfg.STT.Providers[cfg.STT.Provider] = provider
	built := false
	_, err := BuildTranscriber(context.Background(), cfg, BuildOptions{BuildingProvider: func(string) { built = true }})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "même origine") {
		t.Fatalf("BuildTranscriber() error = %v", err)
	}
	if built {
		t.Fatal("provider construction started before health endpoint validation")
	}
}

func TestBuildTranscriberCredentialHelperEnvironment(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "environment")
	helper := filepath.Join(dir, "credential-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nenv > \"$1\"\nprintf token\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	primary := cfg.STT.Providers[cfg.STT.Provider]
	primary.APIKeyCommand = []string{helper, dump}
	cfg.STT.Providers[cfg.STT.Provider] = primary
	fallback := primary
	fallback.APIKeyCommand = nil
	fallback.APIKeyEnv = "MURMUR_CORE_FALLBACK_KEY"
	cfg.STT.Fallback = "fallback"
	cfg.STT.Providers["fallback"] = fallback
	inactive := primary
	inactive.APIKeyCommand = nil
	inactive.APIKeyEnv = "MURMUR_CORE_SHARED_KEY"
	cfg.STT.Providers["inactive"] = inactive
	cfg.Cleanup.Providers["inactive"] = config.CleanupProviderConfig{
		Type: "openai-compatible", BaseURL: "http://127.0.0.1:8000/v1", Model: "cleanup",
		TimeoutS: 10, Zone: "local", APIKeyEnv: "MURMUR_CORE_SHARED_KEY",
	}
	cfg.Cleanup.Providers["case"] = config.CleanupProviderConfig{
		Type: "openai-compatible", BaseURL: "http://127.0.0.1:8000/v1", Model: "cleanup",
		TimeoutS: 10, Zone: "local", APIKeyEnv: "murmur_core_shared_key",
	}

	for name, value := range map[string]string{
		"MURMUR_CORE_FALLBACK_KEY": "fallback-secret",
		"MURMUR_CORE_SHARED_KEY":   "shared-secret",
		"murmur_core_shared_key":   "lower-secret",
		"Murmur_Core_Shared_Key":   "unconfigured-case",
		"HOME":                     "/home/helper-test",
		"XDG_RUNTIME_DIR":          "/run/user/helper-test",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/helper-test/bus",
		"GNUPGHOME":                "/home/helper-test/.gnupg",
		"SSH_AUTH_SOCK":            "/run/user/helper-test/agent",
		"PASSWORD_STORE_DIR":       "/home/helper-test/.password-store",
	} {
		t.Setenv(name, value)
	}

	if _, err := BuildTranscriber(context.Background(), cfg, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	environment := make(map[string]string)
	for _, assignment := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, _ := strings.Cut(assignment, "=")
		environment[name] = value
	}
	for _, name := range []string{"MURMUR_CORE_FALLBACK_KEY", "MURMUR_CORE_SHARED_KEY", "murmur_core_shared_key"} {
		if _, ok := environment[name]; ok {
			t.Fatalf("credential %s reached helper", name)
		}
	}
	for name, want := range map[string]string{
		"Murmur_Core_Shared_Key":   "unconfigured-case",
		"HOME":                     "/home/helper-test",
		"XDG_RUNTIME_DIR":          "/run/user/helper-test",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/helper-test/bus",
		"GNUPGHOME":                "/home/helper-test/.gnupg",
		"SSH_AUTH_SOCK":            "/run/user/helper-test/agent",
		"PASSWORD_STORE_DIR":       "/home/helper-test/.password-store",
	} {
		if got := environment[name]; got != want {
			t.Fatalf("helper environment %s = %q, want %q", name, got, want)
		}
	}
}

type failingProvider struct {
	requests []stt.Request
}

func (p *failingProvider) Name() string { return "failing" }

func (p *failingProvider) Transcribe(_ context.Context, request stt.Request) (stt.Result, error) {
	p.requests = append(p.requests, request)
	return stt.Result{}, errs.New(errs.ServerError, "test", "échec attendu")
}

func TestTranscribeFileCleansStableSourcesAndSegmentsOnProviderError(t *testing.T) {
	for _, chunkDuration := range []time.Duration{0, time.Second} {
		t.Run(chunkDuration.String(), func(t *testing.T) {
			path := coreWAV(t, 2*time.Second)
			provider := &failingProvider{}
			chain, err := stt.NewChain(provider, nil, stt.MultiOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = TranscribeFile(context.Background(), path, chunkDuration, chain, stt.Progress{})
			if errs.Classify(err) != errs.ServerError {
				t.Fatalf("TranscribeFile() error=%v", err)
			}
			if len(provider.requests) == 0 {
				t.Fatal("aucune requête capturée")
			}
			for _, request := range provider.requests {
				if _, handle, err := request.Audio.OpenRIFF(); err == nil {
					handle.Close()
					t.Fatalf("source encore ouverte après erreur: %s", request.Audio.Path())
				}
				if request.Audio.Path() != path {
					if _, err := os.Stat(request.Audio.Path()); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("segment non supprimé: %s (%v)", request.Audio.Path(), err)
					}
					if _, err := os.Stat(filepath.Dir(request.Audio.Path())); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("répertoire de découpage non supprimé: %v", err)
					}
				}
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("le fichier source a été pris en propriété: %v", err)
			}
		})
	}
}

func TestTranscribeFileRejectsSegmentLimitBeforeProviderCalls(t *testing.T) {
	path := coreWAV(t, 3*time.Second)
	provider := &failingProvider{}
	chain, err := stt.NewChain(provider, nil, stt.MultiOptions{MaxSegments: 2})
	if err != nil {
		t.Fatal(err)
	}
	result, err := TranscribeFile(context.Background(), path, time.Second, chain, stt.Progress{})
	if errs.Classify(err) != errs.ServerError || err.Error() != "stt: "+stt.SegmentLimitExceeded {
		t.Fatalf("TranscribeFile() result=%#v error=%v", result, err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("provider requests=%d", len(provider.requests))
	}
}

func coreWAV(t *testing.T, duration time.Duration) string {
	t.Helper()
	dataSize := int(wav.ByteRate * int(duration/time.Second))
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(dataSize+36))
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
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))
	path := filepath.Join(t.TempDir(), "source.wav")
	if err := os.WriteFile(path, append(header, make([]byte, dataSize)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
