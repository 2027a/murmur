package session

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

func TestProcessExecutorsFilterCredentialEnvironment(t *testing.T) {
	cfg := config.Defaults()
	stt := cfg.STT.Providers[cfg.STT.Provider]
	stt.APIKeyEnv = "MURMUR_STT_SECRET"
	cfg.STT.Providers[cfg.STT.Provider] = stt
	cfg.Cleanup.Providers["inactive"] = config.CleanupProviderConfig{
		APIKeyEnv: "MURMUR_CLEANUP_SECRET",
	}
	t.Setenv("MURMUR_STT_SECRET", "stt-value")
	t.Setenv("MURMUR_CLEANUP_SECRET", "cleanup-value")
	t.Setenv("MURMUR_SAFE_ENV", "safe-value")

	external, secret := processExecutors(cfg, nil, nil)
	args := []string{"-c", `printf '%s|%s|%s' "$MURMUR_STT_SECRET" "$MURMUR_CLEANUP_SECRET" "$MURMUR_SAFE_ENV"`}
	got, err := external.Run(context.Background(), "sh", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "||safe-value" {
		t.Fatalf("desktop helper environment = %q", got)
	}
	got, err = secret.Run(context.Background(), "sh", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "||safe-value" {
		t.Fatalf("credential helper environment = %q (parent PATH=%q)", got, os.Getenv("PATH"))
	}
}

func TestBuildRejectsDeterministicErrorsBeforeCredentialHelper(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*BuildOptions)
	}{
		{"invalid config", func(options *BuildOptions) {
			provider := options.Config.STT.Providers[options.Config.STT.Provider]
			provider.Model = ""
			options.Config.STT.Providers[options.Config.STT.Provider] = provider
		}},
		{"missing runtime", func(options *BuildOptions) { options.RuntimeDir = "" }},
		{"missing session ID", func(options *BuildOptions) { options.SessionID = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			provider := cfg.STT.Providers[cfg.STT.Provider]
			provider.APIKeyCommand = []string{"credential-canary"}
			cfg.STT.Providers[cfg.STT.Provider] = provider
			var calls atomic.Int32
			executor := &sysexec.FakeExecutor{RunFunc: func(context.Context, string, []string, []byte) ([]byte, error) {
				calls.Add(1)
				return []byte("secret"), nil
			}}
			options := BuildOptions{
				Config: cfg, RuntimeDir: t.TempDir(), SessionID: "session", SecretExecutor: executor,
			}
			test.edit(&options)
			if _, err := Build(options); errs.Classify(err) != errs.ConfigInvalid {
				t.Fatalf("Build() error = %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("credential helper calls = %d", calls.Load())
			}
		})
	}
}

type countingBuildRecorder struct {
	starts atomic.Int32
}

func (r *countingBuildRecorder) Start(context.Context, audio.RecordOptions) (audio.Recording, error) {
	r.starts.Add(1)
	return nil, nil
}

func TestBuildRejectsTransientPasteBeforeCapture(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sink.Mode = "paste"
	cfg.Sink.ClipboardMode = "transient"
	recorder := &countingBuildRecorder{}

	_, err := Build(BuildOptions{
		Config: cfg, RuntimeDir: t.TempDir(), SessionID: "session", Recorder: recorder,
	})
	if errs.Classify(err) != errs.ConfigInvalid || recorder.starts.Load() != 0 {
		t.Fatalf("Build() error = %v, recorder starts = %d", err, recorder.starts.Load())
	}
}
