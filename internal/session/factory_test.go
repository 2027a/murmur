package session

import (
	"context"
	"os"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/config"
)

func TestProcessExecutorsIsolateCredentialEnvironment(t *testing.T) {
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
	if string(got) != "stt-value|cleanup-value|safe-value" {
		t.Fatalf("credential helper environment = %q (parent PATH=%q)", got, os.Getenv("PATH"))
	}
}
