package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/httpurl"
	"git.2027a.net/2027a/murmur/internal/policy"
)

const (
	MaximumChunkWorkers             = 64
	MaximumTextBytes                = 64 << 20
	MaximumSegments                 = 65536
	MaximumDurationSeconds          = 24 * 60 * 60
	MaximumDurationMilliseconds     = MaximumDurationSeconds * 1000
	MaximumProviderRetries          = 10
	MaximumRetryBackoffMilliseconds = 60 * 1000
	MaximumLogBytes                 = 16 << 20
	MaximumLogFiles                 = 10
	MaximumSessionJournalBytes      = 1 << 30
	MaximumSessionJournalFiles      = 100
)

func Validate(cfg Config) error {
	if _, ok := profiles[cfg.General.Profile]; !ok {
		return configError("general.profile doit valoir offline, local, lan, lan-cloud-cleanup, cloud ou cloud-no-history")
	}
	if !oneOf(cfg.General.CommandPack, "auto", "fr", "en") {
		return configError("general.command_pack doit valoir auto, fr ou en")
	}
	if cfg.STT.ChunkSeconds < 0 || cfg.STT.ChunkSeconds > MaximumDurationSeconds {
		return configError("stt.chunk_seconds doit être compris entre 0 et 86400")
	}
	if cfg.STT.LiveChunking && cfg.STT.ChunkSeconds == 0 {
		return configError("stt.live_chunking exige stt.chunk_seconds strictement positif")
	}
	if cfg.STT.ChunkPauseMS < 0 || cfg.STT.ChunkPauseMS > 1000 || (cfg.STT.ChunkSeconds > 0 && cfg.STT.ChunkPauseMS >= cfg.STT.ChunkSeconds*1000) {
		return configError("stt.chunk_pause_ms doit être compris entre 0 et 1000 et inférieur à la durée du segment")
	}
	if cfg.STT.ChunkOverlapMS < 0 || cfg.STT.ChunkOverlapMS > 1000 || (cfg.STT.ChunkSeconds > 0 && cfg.STT.ChunkOverlapMS >= cfg.STT.ChunkSeconds*1000-cfg.STT.ChunkPauseMS) {
		return configError("stt.chunk_overlap_ms doit être compris entre 0 et 1000 et inférieur au pas minimal du segment")
	}
	if cfg.STT.ChunkWorkers < 1 || cfg.STT.ChunkWorkers > MaximumChunkWorkers {
		return configError("stt.chunk_workers doit être compris entre 1 et 64")
	}
	if cfg.STT.MaxTextBytes < 1 || cfg.STT.MaxTextBytes > MaximumTextBytes {
		return configError("stt.max_text_bytes doit être compris entre 1 et 67108864")
	}
	if cfg.STT.MaxSegments < 1 || cfg.STT.MaxSegments > MaximumSegments {
		return configError("stt.max_segments doit être compris entre 1 et 65536")
	}
	if !oneOf(cfg.Plugin.Mode, "plain", "markdown") {
		return configError("plugin.mode doit valoir plain ou markdown")
	}
	if cfg.Plugin.TimeoutS < 1 || cfg.Plugin.TimeoutS > MaximumDurationSeconds {
		return configError("plugin.timeout_s doit être compris entre 1 et 86400")
	}
	if !oneOf(cfg.Sink.Mode, "paste", "type", "clipboard", "stdout", "file", "auto") {
		return configError("sink.mode invalide")
	}
	if !oneOf(cfg.Sink.AutoMode, "paste", "type") {
		return configError("sink.auto_mode doit valoir paste ou type")
	}
	if !oneOf(cfg.Sink.PasteBackend, "wtype", "hyprland") {
		return configError("sink.paste_backend doit valoir wtype ou hyprland")
	}
	if err := validatePasteRules(cfg.Sink); err != nil {
		return err
	}
	if !oneOf(cfg.Sink.ClipboardMode, "raw-and-clean", "clean-only", "none", "transient") {
		return configError("sink.clipboard_mode invalide")
	}
	if cfg.Sink.Mode == "clipboard" && !oneOf(cfg.Sink.ClipboardMode, "raw-and-clean", "clean-only") {
		return configError("sink.mode=clipboard exige sink.clipboard_mode raw-and-clean ou clean-only")
	}
	for i, shortcut := range cfg.Sink.PasteShortcuts {
		if !oneOf(shortcut, "ctrl+shift+v", "shift+insert", "ctrl+v") {
			return configError(fmt.Sprintf("sink.paste_shortcuts[%d] contient un raccourci inconnu: %q", i, shortcut))
		}
	}
	effectivePaste := cfg.Sink.Mode == "paste" || (cfg.Sink.Mode == "auto" && cfg.Sink.AutoMode == "paste")
	if effectivePaste && cfg.Sink.ClipboardMode == "transient" && len(cfg.Sink.PasteShortcuts) > 0 {
		return configError("sink.clipboard_mode=transient est incompatible avec un sink effectif paste: l’injecteur ne confirme pas la consommation du collage")
	}
	if !oneOf(cfg.Sink.TypeNewlines, "space", "marker", "dangerous-keep") {
		return configError("sink.type_newlines doit valoir space, marker ou dangerous-keep")
	}
	if cfg.Sink.Mode == "file" && strings.TrimSpace(cfg.Sink.File) == "" {
		return configError("sink.file est obligatoire lorsque sink.mode vaut file")
	}
	if !oneOf(cfg.UI.Mode, "auto", "quickshell", "osd", "notify", "none") {
		return configError("ui.mode doit valoir auto, quickshell, osd, notify ou none")
	}
	if strings.TrimSpace(cfg.UI.OSD.Helper) == "" {
		return configError("ui.osd.helper ne peut pas être vide")
	}
	if !oneOf(cfg.UI.OSD.Position, "top-left", "top-center", "center") {
		return configError("ui.osd.position doit valoir top-left, top-center ou center")
	}
	if cfg.UI.OSD.SuccessTimeoutMS < 0 || cfg.UI.OSD.SuccessTimeoutMS > MaximumDurationMilliseconds ||
		cfg.UI.OSD.ErrorTimeoutMS < 0 || cfg.UI.OSD.ErrorTimeoutMS > MaximumDurationMilliseconds {
		return configError("les délais ui.osd doivent être compris entre 0 et 86400000 ms")
	}
	if !oneOf(cfg.Logging.Level, "debug", "info", "warn", "error") {
		return configError("logging.level doit valoir debug, info, warn ou error")
	}
	if cfg.Logging.MaxBytes < 1024 || cfg.Logging.MaxBytes > MaximumLogBytes {
		return configError("logging.max_bytes doit être compris entre 1024 et 16777216")
	}
	if cfg.Logging.MaxFiles < 1 || cfg.Logging.MaxFiles > MaximumLogFiles {
		return configError("logging.max_files doit être compris entre 1 et 10")
	}
	if cfg.Logging.SessionMaxBytes < 1024 || cfg.Logging.SessionMaxBytes > MaximumSessionJournalBytes {
		return configError("logging.session_max_bytes doit être compris entre 1024 et 1073741824")
	}
	if cfg.Logging.SessionMaxFiles < 1 || cfg.Logging.SessionMaxFiles > MaximumSessionJournalFiles {
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
		return policyError("le plugin sélectionné utilise la zone local, interdite par policy.plugin_zones")
	}
	trusted := make([]netip.Prefix, 0, len(cfg.Policy.TrustedNetworks))
	for _, value := range cfg.Policy.TrustedNetworks {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return configError(fmt.Sprintf("policy.trusted_networks contient un CIDR invalide: %s", value))
		}
		trusted = append(trusted, prefix)
	}

	if _, ok := cfg.STT.Providers[cfg.STT.Provider]; !ok || cfg.STT.Provider == "" {
		return configError(fmt.Sprintf("stt.provider référence une entrée absente: %q", cfg.STT.Provider))
	}
	if cfg.STT.Fallback != "" {
		if cfg.STT.Fallback == cfg.STT.Provider {
			return configError("stt.fallback doit être distinct de stt.provider")
		}
		if _, ok := cfg.STT.Providers[cfg.STT.Fallback]; !ok {
			return configError(fmt.Sprintf("stt.fallback référence une entrée absente: %q", cfg.STT.Fallback))
		}
	}
	for _, name := range orderedProviderNames(cfg.STT.Providers, cfg.STT.Provider, cfg.STT.Fallback) {
		if err := validateProvider("stt.providers."+name, cfg.STT.Providers[name], trusted); err != nil {
			return err
		}
	}
	primary := cfg.STT.Providers[cfg.STT.Provider]
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

	if cfg.Cleanup.Enabled && cfg.Cleanup.Provider == "" {
		return configError("cleanup.provider est obligatoire lorsque cleanup.enabled vaut true")
	}
	if cfg.Cleanup.Provider != "" {
		_, ok := cfg.Cleanup.Providers[cfg.Cleanup.Provider]
		if !ok {
			return configError(fmt.Sprintf("cleanup.provider référence une entrée absente: %q", cfg.Cleanup.Provider))
		}
	}
	for _, name := range orderedProviderNames(cfg.Cleanup.Providers, cfg.Cleanup.Provider) {
		if err := validateCleanupProvider("cleanup.providers."+name, cfg.Cleanup.Providers[name], trusted); err != nil {
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

func validateProvider(path string, provider ProviderConfig, trusted []netip.Prefix) error {
	if strings.TrimSpace(provider.Model) == "" {
		return configError(path + ".model ne peut pas être vide")
	}
	if err := validateHTTPProvider(path, provider.Type, provider.BaseURL, provider.TimeoutS, provider.Zone, provider.AllowInsecure, provider.credentials(), trusted); err != nil {
		return err
	}
	if provider.Retries < 0 || provider.Retries > MaximumProviderRetries {
		return configError(path + ".retries doit être compris entre 0 et 10")
	}
	if provider.RetryBackoffMS < 0 || provider.RetryBackoffMS > MaximumRetryBackoffMilliseconds {
		return configError(path + ".retry_backoff_ms doit être compris entre 0 et 60000")
	}
	if provider.HealthEnabled && strings.TrimSpace(provider.HealthEndpoint) == "" {
		return configError(path + ".health_endpoint est obligatoire lorsque health_enabled vaut true")
	}
	if provider.HealthEndpoint != "" {
		base, _ := url.Parse(provider.BaseURL)
		endpoint, err := httpurl.ResolveEndpoint(base, provider.HealthEndpoint)
		if err != nil {
			return configError(path + ".health_endpoint doit être une URL HTTP(S) ou un chemin valide, sans userinfo ni fragment")
		}
		if provider.Zone == "internet" && endpoint.Scheme != "https" && !provider.AllowInsecure {
			return configError(path + ".health_endpoint doit utiliser HTTPS en zone internet, sauf allow_insecure=true")
		}
		if !httpurl.SameOrigin(base, endpoint) {
			return configError(path + ".health_endpoint doit conserver la même origine que base_url")
		}
	}
	return nil
}

func validateCleanupProvider(path string, provider CleanupProviderConfig, trusted []netip.Prefix) error {
	if strings.TrimSpace(provider.Model) == "" {
		return configError(path + ".model ne peut pas être vide")
	}
	if !oneOf(provider.ReasoningEffort, "", "none", "minimal", "low", "medium", "high", "xhigh", "max") {
		return configError(path + ".reasoning_effort doit être vide ou valoir none, minimal, low, medium, high, xhigh ou max")
	}
	return validateHTTPProvider(path, provider.Type, provider.BaseURL, provider.TimeoutS, provider.Zone, provider.AllowInsecure, provider.credentials(), trusted)
}

func validateHTTPProvider(path, providerType, baseURL string, timeoutS int, zone string, allowInsecure bool, credentials CredentialConfig, trusted []netip.Prefix) error {
	if providerType != "openai-compatible" {
		return configError(path + ".type doit valoir openai-compatible")
	}
	if timeoutS < 1 || timeoutS > MaximumDurationSeconds {
		return configError(path + ".timeout_s doit être compris entre 1 et 86400")
	}
	if !oneOf(zone, "local", "lan", "internet") {
		return configError(path + ".zone doit valoir local, lan ou internet")
	}
	if secretSourceCount(credentials) > 1 {
		return configError(path + " ne peut définir qu'une source parmi api_key_env, api_key_file et api_key_command")
	}
	if credentials.APIKeyFile != "" && !filepath.IsAbs(credentials.APIKeyFile) {
		return configError(path + ".api_key_file doit être un chemin absolu")
	}
	if len(credentials.APIKeyCommand) > 0 && credentials.APIKeyCommand[0] == "" {
		return configError(path + ".api_key_command doit commencer par un exécutable non vide")
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
	// Classify literal IPs with the transport's rules and zone ceiling. Do not
	// resolve hostnames here: DNS destinations are checked again at dial time.
	if zone == "local" && !(strings.EqualFold(host, "localhost") || (ip != nil && policy.Classify(ip, trusted) == policy.Local)) {
		return configError(path + " déclare une zone local mais base_url n'est pas locale")
	}
	if zone == "lan" && ip != nil && policy.Classify(ip, trusted) > policy.LAN {
		return configError(path + " déclare une zone lan mais base_url contient une IP publique non fiable")
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

func orderedProviderNames[T any](providers map[string]T, preferred ...string) []string {
	names := make([]string, 0, len(providers))
	seen := make(map[string]bool, len(providers))
	for _, name := range preferred {
		if name == "" || seen[name] {
			continue
		}
		if _, ok := providers[name]; ok {
			names = append(names, name)
			seen[name] = true
		}
	}
	remaining := make([]string, 0, len(providers)-len(names))
	for name := range providers {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(names, remaining...)
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
