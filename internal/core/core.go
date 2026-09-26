// Package core assembles the provider and text-processing core shared by all
// Murmur frontends. It deliberately has no capture, UI, clipboard or sink code.
package core

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"time"

	"git.2027a.net/2027a/murmur/internal/cleanup"
	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/plugin"
	markdownplugin "git.2027a.net/2027a/murmur/internal/plugin/markdown"
	"git.2027a.net/2027a/murmur/internal/policy"
	"git.2027a.net/2027a/murmur/internal/stt"
	"git.2027a.net/2027a/murmur/internal/sysexec"
	"git.2027a.net/2027a/murmur/internal/textpipe"
)

type BuildOptions struct {
	Executor         sysexec.Executor
	BuildingProvider func(string)
}

func BuildTranscriber(ctx context.Context, cfg config.Config, options BuildOptions) (*stt.Chain, error) {
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	trusted, err := trustedNetworks(cfg.Policy.TrustedNetworks)
	if err != nil {
		return nil, err
	}
	resolver := secretResolver(cfg, options.Executor)
	building(options, cfg.STT.Provider)
	primary, err := buildSTT(ctx, cfg.STT.Provider, cfg.STT.Providers[cfg.STT.Provider], trusted, resolver)
	if err != nil {
		return nil, err
	}
	var fallback stt.Provider
	if cfg.STT.Fallback != "" {
		building(options, cfg.STT.Fallback)
		fallback, err = buildSTT(ctx, cfg.STT.Fallback, cfg.STT.Providers[cfg.STT.Fallback], trusted, resolver)
		if err != nil {
			return nil, err
		}
	}
	return stt.NewChain(primary, fallback, stt.MultiOptions{
		AllowMixedProviders: cfg.STT.AllowMixedProviders,
		AllowPartial:        cfg.STT.AllowPartial,
		Workers:             cfg.STT.ChunkWorkers,
		MaxTextBytes:        cfg.STT.MaxTextBytes,
		MaxSegments:         cfg.STT.MaxSegments,
	})
}

func BuildCleanup(ctx context.Context, cfg config.Config, options BuildOptions) (*cleanup.Client, error) {
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	if !cfg.Cleanup.Enabled {
		return nil, nil
	}
	trusted, err := trustedNetworks(cfg.Policy.TrustedNetworks)
	if err != nil {
		return nil, err
	}
	building(options, cfg.Cleanup.Provider)
	provider := cfg.Cleanup.Providers[cfg.Cleanup.Provider]
	secret, err := secretResolver(cfg, options.Executor).ResolveCleanup(ctx, provider)
	if err != nil {
		return nil, err
	}
	zone, err := policy.ParseZone(provider.Zone)
	if err != nil {
		return nil, err
	}
	return cleanup.NewOpenAICompatible(cleanup.Config{
		BaseURL: provider.BaseURL, Model: provider.Model, Prompt: cfg.Cleanup.Prompt,
		APIKey: secret, Timeout: time.Duration(provider.TimeoutS) * time.Second,
		AllowedZone: zone, AllowInsecure: provider.AllowInsecure, TrustedNetworks: trusted,
	})
}

func BuildProcessor(ctx context.Context, cfg config.Config, options BuildOptions) (*textpipe.Pipeline, error) {
	client, err := BuildCleanup(ctx, cfg, options)
	if err != nil {
		return nil, err
	}
	var cleaner textpipe.Cleanup
	if client != nil {
		cleaner = textpipe.CleanupFunc(func(ctx context.Context, text string, _ textpipe.Protection) (string, error) {
			result := client.Clean(ctx, text)
			return result.Text, result.Degraded
		})
	}
	grammar, err := BuildPlugin(cfg)
	if err != nil {
		return nil, err
	}
	return textpipe.New(textpipe.Options{
		CommandPack:     cfg.General.CommandPack,
		STTLanguage:     cfg.STT.Providers[cfg.STT.Provider].Language,
		ASCIIQuotes:     cfg.General.ASCIIQuotes,
		CleanupProvider: cfg.Cleanup.Provider,
		Cleanup:         cleaner,
		Plugin:          grammar,
	})
}

func BuildPlugin(cfg config.Config) (textpipe.Plugin, error) {
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	if !cfg.Plugin.Enabled {
		return nil, nil
	}
	var implementation plugin.Definition
	switch cfg.Plugin.Mode {
	case "plain":
		implementation = plugin.Plain{}
	case "markdown":
		implementation = markdownplugin.Plugin{}
	default:
		return nil, errs.New(errs.ConfigInvalid, "plugin", fmt.Sprintf("mode inconnu: %q", cfg.Plugin.Mode))
	}
	zones := make([]policy.Zone, len(cfg.Policy.PluginZones))
	for i, value := range cfg.Policy.PluginZones {
		zone, err := policy.ParseZone(value)
		if err != nil {
			return nil, err
		}
		zones[i] = zone
	}
	trusted, err := trustedNetworks(cfg.Policy.TrustedNetworks)
	if err != nil {
		return nil, err
	}
	return plugin.New(implementation, plugin.Options{
		Timeout:         time.Duration(cfg.Plugin.TimeoutS) * time.Second,
		AllowedZones:    zones,
		TrustedNetworks: trusted,
	})
}

func buildSTT(ctx context.Context, name string, provider config.ProviderConfig, trusted []netip.Prefix, resolver config.SecretResolver) (stt.Provider, error) {
	secret, err := resolver.Resolve(ctx, provider)
	if err != nil {
		return nil, err
	}
	return NewSTTProvider(name, provider, secret, trusted)
}

// NewSTTProvider is the shared config-to-provider contract used by sessions and doctor.
func NewSTTProvider(name string, provider config.ProviderConfig, secret string, trusted []netip.Prefix) (*stt.OpenAICompatible, error) {
	zone, err := policy.ParseZone(provider.Zone)
	if err != nil {
		return nil, err
	}
	return stt.NewOpenAICompatible(stt.Config{
		Name: name, BaseURL: provider.BaseURL, Model: provider.Model, Language: provider.Language,
		Prompt: provider.Prompt, APIKey: secret, Timeout: time.Duration(provider.TimeoutS) * time.Second,
		AllowedZone: zone, TrustedNetworks: trusted,
		AllowInsecure: provider.AllowInsecure,
		HealthEnabled: provider.HealthEnabled, HealthEndpoint: provider.HealthEndpoint,
		Retries: provider.Retries, RetryBackoff: time.Duration(provider.RetryBackoffMS) * time.Millisecond,
	})
}

func trustedNetworks(values []string) ([]netip.Prefix, error) {
	result := make([]netip.Prefix, len(values))
	for i, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("policy.trusted_networks: %w", err)
		}
		result[i] = prefix
	}
	return result, nil
}

func secretResolver(cfg config.Config, executor sysexec.Executor) config.SecretResolver {
	if executor == nil {
		executor = sysexec.OSExecutor{Env: config.CredentialHelperEnvironment(cfg, os.Environ())}
	}
	return config.SecretResolver{Runner: commandRunner{executor: executor}}
}

func building(options BuildOptions, provider string) {
	if options.BuildingProvider != nil {
		options.BuildingProvider(provider)
	}
}

type commandRunner struct{ executor sysexec.Executor }

func (r commandRunner) Run(ctx context.Context, name string, args []string) ([]byte, error) {
	return sysexec.RunLimited(ctx, r.executor, name, args, nil, config.MaxSecretBytes)
}
