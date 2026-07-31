package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/session"
	"git.2027a.net/mathieu/murmur/internal/xdg"
)

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

func TestCleanupDisabledPassesStdinThrough(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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

func TestCleanupInputLimitIsStructured(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
	if got.Active || got.State != "idle" || got.LastResult == nil || got.LastResult.Result != session.Cancelled {
		t.Fatalf("status=%#v", got)
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
