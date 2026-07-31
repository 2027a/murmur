package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

func Validate(cfg Config) error {
	if _, ok := profiles[cfg.General.Profile]; !ok {
		return configError("general.profile doit valoir offline, local, lan, lan-cloud-cleanup, cloud ou cloud-no-history")
	}
	if !oneOf(cfg.General.CommandPack, "auto", "fr", "en") {
		return configError("general.command_pack doit valoir auto, fr ou en")
	}
	if cfg.STT.ChunkSeconds < 0 {
		return configError("stt.chunk_seconds ne peut pas être négatif")
	}
	if cfg.STT.LiveChunking && cfg.STT.ChunkSeconds == 0 {
		return configError("stt.live_chunking exige stt.chunk_seconds strictement positif")
	}
	if cfg.STT.ChunkWorkers < 1 || cfg.STT.ChunkWorkers > 64 {
		return configError("stt.chunk_workers doit être compris entre 1 et 64")
	}
	if !oneOf(cfg.Plugin.Mode, "plain", "markdown") {
		return configError("plugin.mode doit valoir plain ou markdown")
	}
	if cfg.Plugin.TimeoutS <= 0 {
		return configError("plugin.timeout_s doit être strictement positif")
	}
	if !oneOf(cfg.Sink.Mode, "paste", "type", "clipboard", "stdout", "file", "auto") {
		return configError("sink.mode invalide")
	}
	if !oneOf(cfg.Sink.AutoMode, "paste", "type") {
		return configError("sink.auto_mode doit valoir paste ou type")
	}
	if !oneOf(cfg.Sink.ClipboardMode, "raw-and-clean", "clean-only", "none", "transient") {
		return configError("sink.clipboard_mode invalide")
	}
	if !oneOf(cfg.Sink.TypeNewlines, "keep", "space", "marker") {
		return configError("sink.type_newlines doit valoir keep, space ou marker")
	}
	if cfg.Sink.Mode == "file" && strings.TrimSpace(cfg.Sink.File) == "" {
		return configError("sink.file est obligatoire lorsque sink.mode vaut file")
	}
	if !oneOf(cfg.UI.Mode, "osd", "notify", "none") {
		return configError("ui.mode doit valoir osd, notify ou none")
	}
	if strings.TrimSpace(cfg.UI.OSD.Helper) == "" {
		return configError("ui.osd.helper ne peut pas être vide")
	}
	if !oneOf(cfg.UI.OSD.Position, "top-left", "top-center", "center") {
		return configError("ui.osd.position doit valoir top-left, top-center ou center")
	}
	if cfg.UI.OSD.SuccessTimeoutMS < 0 || cfg.UI.OSD.ErrorTimeoutMS < 0 {
		return configError("les délais ui.osd ne peuvent pas être négatifs")
	}
	if !oneOf(cfg.Logging.Level, "debug", "info", "warn", "error") {
		return configError("logging.level doit valoir debug, info, warn ou error")
	}
	if cfg.Logging.SessionMaxBytes < 1024 {
		return configError("logging.session_max_bytes doit être supérieur ou égal à 1024")
	}
	if cfg.Logging.SessionMaxFiles < 1 || cfg.Logging.SessionMaxFiles > 100 {
		return configError("logging.session_max_files doit être compris entre 1 et 100")
	}
	if err := validateZoneList("policy.stt_zones", cfg.Policy.STTZones); err != nil {
		return err
	}
	if err := validateZoneList("policy.cleanup_zones", cfg.Policy.CleanupZones); err != nil {
		return err
	}
	if err := validateZoneList("policy.plugin_zones", cfg.Policy.PluginZones); err != nil {
		return err
	}
	if cfg.Plugin.Enabled && !contains(cfg.Policy.PluginZones, "local") {
		return configError("le plugin sélectionné utilise la zone local, interdite par policy.plugin_zones")
	}
	for _, prefix := range cfg.Policy.TrustedNetworks {
		if _, err := netip.ParsePrefix(prefix); err != nil {
			return configError(fmt.Sprintf("policy.trusted_networks contient un CIDR invalide: %s", prefix))
		}
	}

	primary, ok := cfg.STT.Providers[cfg.STT.Provider]
	if !ok || cfg.STT.Provider == "" {
		return configError(fmt.Sprintf("stt.provider référence une entrée absente: %q", cfg.STT.Provider))
	}
	if err := validateProvider("stt.providers."+cfg.STT.Provider, primary); err != nil {
		return err
	}
	if cfg.STT.Fallback != "" {
		if cfg.STT.Fallback == cfg.STT.Provider {
			return configError("stt.fallback doit être distinct de stt.provider")
		}
		fallback, ok := cfg.STT.Providers[cfg.STT.Fallback]
		if !ok {
			return configError(fmt.Sprintf("stt.fallback référence une entrée absente: %q", cfg.STT.Fallback))
		}
		if err := validateProvider("stt.providers."+cfg.STT.Fallback, fallback); err != nil {
			return err
		}
	}
	if cfg.General.CommandPack == "auto" {
		primaryLanguage := commandLanguage(primary.Language)
		if !oneOf(primaryLanguage, "fr", "en") {
			return configError("general.command_pack=auto exige une langue fr ou en explicite sur le provider STT principal")
		}
		if cfg.STT.Fallback != "" {
			fallbackLanguage := commandLanguage(cfg.STT.Providers[cfg.STT.Fallback].Language)
			if fallbackLanguage != primaryLanguage {
				return configError("general.command_pack=auto exige la même langue sur stt.provider et stt.fallback")
			}
		}
	}
	for name, provider := range cfg.STT.Providers {
		if err := validateProvider("stt.providers."+name, provider); err != nil {
			return err
		}
	}

	if cfg.Cleanup.Enabled && cfg.Cleanup.Provider == "" {
		return configError("cleanup.provider est obligatoire lorsque cleanup.enabled vaut true")
	}
	if cfg.Cleanup.Provider != "" {
		_, ok := cfg.Cleanup.Providers[cfg.Cleanup.Provider]
		if !ok {
			return configError(fmt.Sprintf("cleanup.provider référence une entrée absente: %q", cfg.Cleanup.Provider))
		}
	}
	for name, provider := range cfg.Cleanup.Providers {
		if err := validateCleanupProvider("cleanup.providers."+name, provider); err != nil {
			return err
		}
	}
	return ValidateActiveProviderPolicy(cfg)
}

// ValidateActiveProviderPolicy rejects only providers that can be selected by
// the effective configuration. It is safe to call again at construction time.
func ValidateActiveProviderPolicy(cfg Config) error {
	if provider, ok := cfg.STT.Providers[cfg.STT.Provider]; ok && !contains(cfg.Policy.STTZones, provider.Zone) {
		return policyError(fmt.Sprintf("le provider STT %q utilise la zone %q, interdite par policy.stt_zones", cfg.STT.Provider, provider.Zone))
	}
	if fallback, ok := cfg.STT.Providers[cfg.STT.Fallback]; cfg.STT.Fallback != "" && ok && !contains(cfg.Policy.STTZones, fallback.Zone) {
		return policyError(fmt.Sprintf("le fallback STT %q utilise la zone %q, interdite par policy.stt_zones", cfg.STT.Fallback, fallback.Zone))
	}
	if provider, ok := cfg.Cleanup.Providers[cfg.Cleanup.Provider]; cfg.Cleanup.Enabled && ok && !contains(cfg.Policy.CleanupZones, provider.Zone) {
		return policyError(fmt.Sprintf("le provider cleanup %q utilise la zone %q, interdite par policy.cleanup_zones", cfg.Cleanup.Provider, provider.Zone))
	}
	return nil
}

// ValidateClipboardOnly applies the session-level guarantee before capture.
func ValidateClipboardOnly(cfg Config, enabled bool) error {
	if enabled && (cfg.Sink.ClipboardMode == "none" || cfg.Sink.ClipboardMode == "transient") {
		return configError("clipboard-only exige sink.clipboard_mode raw-and-clean ou clean-only")
	}
	return nil
}

func validateProvider(path string, provider ProviderConfig) error {
	if err := validateHTTPProvider(path, provider.Type, provider.BaseURL, provider.TimeoutS, provider.Zone, provider.AllowInsecure, provider.credentials()); err != nil {
		return err
	}
	if provider.Retries < 0 || provider.Retries > 10 {
		return configError(path + ".retries doit être compris entre 0 et 10")
	}
	if provider.RetryBackoffMS < 0 || provider.RetryBackoffMS > 60000 {
		return configError(path + ".retry_backoff_ms doit être compris entre 0 et 60000")
	}
	if provider.HealthEnabled && strings.TrimSpace(provider.HealthEndpoint) == "" {
		return configError(path + ".health_endpoint est obligatoire lorsque health_enabled vaut true")
	}
	if provider.HealthEndpoint != "" {
		endpoint, err := url.Parse(provider.HealthEndpoint)
		if err != nil || endpoint.User != nil || endpoint.Fragment != "" || (endpoint.IsAbs() && endpoint.Scheme != "http" && endpoint.Scheme != "https") {
			return configError(path + ".health_endpoint doit être une URL HTTP(S) ou un chemin valide, sans userinfo ni fragment")
		}
	}
	return nil
}

func validateCleanupProvider(path string, provider CleanupProviderConfig) error {
	return validateHTTPProvider(path, provider.Type, provider.BaseURL, provider.TimeoutS, provider.Zone, provider.AllowInsecure, provider.credentials())
}

func validateHTTPProvider(path, providerType, baseURL string, timeoutS int, zone string, allowInsecure bool, credentials CredentialConfig) error {
	if providerType != "openai-compatible" {
		return configError(path + ".type doit valoir openai-compatible")
	}
	if timeoutS <= 0 {
		return configError(path + ".timeout_s doit être strictement positif")
	}
	if !oneOf(zone, "local", "lan", "internet") {
		return configError(path + ".zone doit valoir local, lan ou internet")
	}
	if secretSourceCount(credentials) > 1 {
		return configError(path + " ne peut définir qu'une source parmi api_key_env, api_key_file et api_key_command")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return configError(path + ".base_url doit être une URL HTTP(S) absolue")
	}
	if zone == "internet" && parsed.Scheme != "https" && !allowInsecure {
		return configError(path + ".base_url doit utiliser HTTPS en zone internet, sauf allow_insecure=true")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if zone == "local" && !(strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())) {
		return configError(path + " déclare une zone local mais base_url n'est pas locale")
	}
	if zone == "lan" && ip != nil && !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return configError(path + " déclare une zone lan mais base_url contient une IP publique")
	}
	return nil
}

func validateZoneList(path string, zones []string) error {
	if len(zones) == 0 {
		return configError(path + " ne peut pas être vide")
	}
	for _, zone := range zones {
		if !oneOf(zone, "local", "lan", "internet") {
			return configError(path + " contient une zone invalide: " + zone)
		}
	}
	return nil
}

func secretSourceCount(provider CredentialConfig) int {
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

func policyError(message string) error {
	return errs.New(errs.PolicyBlocked, "policy", message)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func oneOf(value string, allowed ...string) bool {
	return contains(allowed, value)
}

func commandLanguage(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "_", "-")))
	if base, _, ok := strings.Cut(value, "-"); ok {
		return base
	}
	return value
}
