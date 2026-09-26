package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args []string) ([]byte, error)
}

type SecretResolver struct {
	LookupEnv      func(string) (string, bool)
	Runner         CommandRunner
	CommandTimeout time.Duration
}

const (
	DefaultSecretCommandTimeout = 10 * time.Second
	MaxSecretBytes              = 64 << 10
)

var credentialFstat = func(fd int) (syscall.Stat_t, error) {
	var stat syscall.Stat_t
	err := syscall.Fstat(fd, &stat)
	return stat, err
}

func (r SecretResolver) Resolve(ctx context.Context, provider ProviderConfig) (string, error) {
	return r.resolve(ctx, provider.credentials())
}

func (r SecretResolver) ResolveCleanup(ctx context.Context, provider CleanupProviderConfig) (string, error) {
	return r.resolve(ctx, provider.credentials())
}

func (r SecretResolver) ResolveCredentials(ctx context.Context, credentials CredentialConfig) (string, error) {
	return r.resolve(ctx, credentials)
}

func (r SecretResolver) resolve(ctx context.Context, provider CredentialConfig) (string, error) {
	if credentialSourceCount(provider) == 0 {
		return "", nil
	}
	if credentialSourceCount(provider) > 1 {
		return "", errs.New(errs.ConfigInvalid, "secret", "plusieurs sources de credential sont configurées")
	}
	if provider.APIKeyEnv != "" {
		lookup := r.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, ok := lookup(provider.APIKeyEnv)
		if !ok || strings.TrimSpace(value) == "" {
			return "", errs.New(errs.AuthFailed, "secret", fmt.Sprintf("variable d'environnement %s absente ou vide", provider.APIKeyEnv))
		}
		return strings.TrimSpace(value), nil
	}
	if provider.APIKeyFile != "" {
		if !filepath.IsAbs(provider.APIKeyFile) {
			return "", errs.New(errs.ConfigInvalid, "secret", "api_key_file doit être un chemin absolu")
		}
		file, err := openAbsoluteFileNoFollow(provider.APIKeyFile)
		if err != nil {
			if errors.Is(err, errUnsafePathComponent) {
				return "", errs.New(errs.ConfigInvalid, "secret", "api_key_file ne peut contenir aucun lien symbolique")
			}
			return "", errs.Wrap(errs.AuthFailed, "secret", "fichier de credential inaccessible", err)
		}
		defer file.Close()
		stat, err := credentialFstat(int(file.Fd()))
		if err != nil {
			return "", errs.Wrap(errs.AuthFailed, "secret", "inspection du fichier de credential impossible", err)
		}
		if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0o077 != 0 || stat.Uid != uint32(os.Geteuid()) {
			return "", errs.New(errs.ConfigInvalid, "secret", "le fichier de credential doit appartenir à l'utilisateur, être régulier et inaccessible au groupe et aux autres")
		}
		if stat.Size > MaxSecretBytes {
			return "", errs.New(errs.AuthFailed, "secret", "le fichier de credential dépasse 64 KiB")
		}
		data, err := io.ReadAll(io.LimitReader(file, MaxSecretBytes+1))
		if err != nil {
			return "", errs.Wrap(errs.AuthFailed, "secret", "lecture du fichier de credential impossible", err)
		}
		if len(data) > MaxSecretBytes {
			return "", errs.New(errs.AuthFailed, "secret", "le fichier de credential dépasse 64 KiB")
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", errs.New(errs.AuthFailed, "secret", "le fichier de credential est vide")
		}
		return value, nil
	}
	if r.Runner == nil {
		return "", errs.New(errs.ConfigInvalid, "secret", "aucun exécuteur n'est disponible pour api_key_command")
	}
	timeout := r.CommandTimeout
	if timeout <= 0 {
		timeout = DefaultSecretCommandTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := r.Runner.Run(commandCtx, provider.APIKeyCommand[0], provider.APIKeyCommand[1:])
	if err != nil {
		class := errs.AuthFailed
		if commandCtx.Err() != nil {
			class = errs.Classify(commandCtx.Err())
		}
		return "", errs.Wrap(class, "secret", "le helper de credential a échoué", err)
	}
	if len(output) > MaxSecretBytes {
		return "", errs.New(errs.AuthFailed, "secret", "la sortie du helper de credential dépasse 64 KiB")
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", errs.New(errs.AuthFailed, "secret", "le helper de credential a renvoyé une valeur vide")
	}
	return value, nil
}

func credentialSourceCount(provider CredentialConfig) int {
	count := 0
	if provider.APIKeyEnv != "" {
		count++
	}
	if provider.APIKeyFile != "" {
		count++
	}
	if len(provider.APIKeyCommand) > 0 {
		count++
	}
	return count
}

// APIKeyEnvironmentNames returns every credential environment variable named
// by the snapshot, including inactive and fallback providers.
func APIKeyEnvironmentNames(cfg Config) []string {
	names := make([]string, 0, len(cfg.STT.Providers)+len(cfg.Cleanup.Providers))
	seen := make(map[string]struct{}, cap(names))
	add := func(name string) {
		if name == "" {
			return
		}
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, provider := range cfg.STT.Providers {
		add(provider.APIKeyEnv)
	}
	for _, provider := range cfg.Cleanup.Providers {
		add(provider.APIKeyEnv)
	}
	return names
}

// CredentialHelperEnvironment preserves the parent environment except for every
// credential variable named by the complete provider snapshot. Unix environment
// names are case-sensitive, and every duplicate assignment is removed.
func CredentialHelperEnvironment(cfg Config, environment []string) []string {
	blocked := make(map[string]struct{})
	for _, name := range APIKeyEnvironmentNames(cfg) {
		blocked[name] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, assignment := range environment {
		name, _, _ := strings.Cut(assignment, "=")
		if _, remove := blocked[name]; remove {
			continue
		}
		filtered = append(filtered, assignment)
	}
	return filtered
}
