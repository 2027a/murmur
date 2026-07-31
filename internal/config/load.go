package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/xdg"
)

type ResolveOptions struct {
	Path      string
	LookupEnv func(string) (string, bool)
	Overrides Overrides
}

// Resolve applies defaults, the TOML file, whitelisted MURMUR_* variables, then flags.
func Resolve(path string, overrides Overrides) (Config, error) {
	return ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: os.LookupEnv, Overrides: overrides})
}

func ResolveWithOptions(options ResolveOptions) (Config, error) {
	cfg := Defaults()
	explicit := make(map[string]bool)
	path := options.Path
	explicitPath := path != ""
	if path == "" {
		dir, err := xdg.ConfigDir()
		if err != nil {
			return Config{}, errs.Wrap(errs.ConfigInvalid, "config", "répertoire de configuration introuvable", err)
		}
		path = filepath.Join(dir, "config.toml")
	}

	file, identity, err := openConfigFile(path)
	if err != nil {
		if !explicitPath && errors.Is(err, os.ErrNotExist) {
			// Defaults form a valid local-only configuration.
		} else {
			return Config{}, errs.Wrap(errs.ConfigInvalid, "config", "lecture TOML impossible", err)
		}
	}
	var metadata toml.MetaData
	if file != nil {
		if identity.size > MaxConfigBytes {
			err = fmt.Errorf("config.toml dépasse 1 Mio")
		}
		var snapshot []byte
		if err == nil {
			snapshot, err = io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
			if err == nil && len(snapshot) > MaxConfigBytes {
				err = fmt.Errorf("config.toml dépasse 1 Mio")
			}
		}
		if err == nil {
			err = verifyConfigFile(file, path, identity)
		}
		if err == nil {
			metadata, err = toml.NewDecoder(bytes.NewReader(snapshot)).Decode(&cfg)
		}
		closeErr := file.Close()
		if err != nil {
			return Config{}, errs.Wrap(errs.ConfigInvalid, "config", "lecture TOML impossible", err)
		}
		if closeErr != nil {
			return Config{}, errs.Wrap(errs.ConfigInvalid, "config", "fermeture du fichier TOML impossible", closeErr)
		}
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return Config{}, configError("clés TOML inconnues: " + strings.Join(keys, ", "))
	} else if file != nil {
		for _, key := range profileDefaultKeys {
			parts := strings.Split(key, ".")
			explicit[key] = metadata.IsDefined(parts...)
		}
	}

	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if err := applyEnvironment(&cfg, lookup, explicit); err != nil {
		return Config{}, err
	}
	applyOverrides(&cfg, options.Overrides, explicit)
	applyProfileDefaults(&cfg, explicit)
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	resolveAutoSink(&cfg)
	return cfg, nil
}

func applyEnvironment(cfg *Config, lookup func(string) (string, bool), explicit map[string]bool) error {
	stringVars := map[string]struct {
		target *string
		key    string
	}{
		"MURMUR_PROFILE":             {&cfg.General.Profile, "general.profile"},
		"MURMUR_COMMAND_PACK":        {&cfg.General.CommandPack, "general.command_pack"},
		"MURMUR_AUDIO_SOURCE":        {&cfg.Audio.Source, "audio.source"},
		"MURMUR_STT_PROVIDER":        {&cfg.STT.Provider, "stt.provider"},
		"MURMUR_STT_FALLBACK":        {&cfg.STT.Fallback, "stt.fallback"},
		"MURMUR_CLEANUP_PROVIDER":    {&cfg.Cleanup.Provider, "cleanup.provider"},
		"MURMUR_PLUGIN_MODE":         {&cfg.Plugin.Mode, "plugin.mode"},
		"MURMUR_SINK_MODE":           {&cfg.Sink.Mode, "sink.mode"},
		"MURMUR_SINK_AUTO_MODE":      {&cfg.Sink.AutoMode, "sink.auto_mode"},
		"MURMUR_SINK_CLIPBOARD_MODE": {&cfg.Sink.ClipboardMode, "sink.clipboard_mode"},
		"MURMUR_SINK_FILE":           {&cfg.Sink.File, "sink.file"},
		"MURMUR_UI_MODE":             {&cfg.UI.Mode, "ui.mode"},
		"MURMUR_OSD_HELPER":          {&cfg.UI.OSD.Helper, "ui.osd.helper"},
		"MURMUR_OSD_POSITION":        {&cfg.UI.OSD.Position, "ui.osd.position"},
		"MURMUR_LOG_LEVEL":           {&cfg.Logging.Level, "logging.level"},
	}
	for name, entry := range stringVars {
		if value, ok := lookup(name); ok {
			*entry.target = value
			explicit[entry.key] = true
		}
	}

	boolVars := map[string]struct {
		target *bool
		key    string
	}{
		"MURMUR_ASCII_QUOTES":              {&cfg.General.ASCIIQuotes, "general.ascii_quotes"},
		"MURMUR_KEEP_RECORDINGS":           {&cfg.Audio.KeepRecordings, "audio.keep_recordings"},
		"MURMUR_STT_LIVE_CHUNKING":         {&cfg.STT.LiveChunking, "stt.live_chunking"},
		"MURMUR_STT_ALLOW_PARTIAL":         {&cfg.STT.AllowPartial, "stt.allow_partial"},
		"MURMUR_STT_ALLOW_MIXED_PROVIDERS": {&cfg.STT.AllowMixedProviders, "stt.allow_mixed_providers"},
		"MURMUR_CLEANUP_ENABLED":           {&cfg.Cleanup.Enabled, "cleanup.enabled"},
		"MURMUR_PLUGIN_ENABLED":            {&cfg.Plugin.Enabled, "plugin.enabled"},
		"MURMUR_RETAIN_DIAGNOSTICS":        {&cfg.Policy.RetainDiagnostics, "policy.retain_diagnostics"},
		"MURMUR_UI_SOUND":                  {&cfg.UI.Sound, "ui.sound"},
		"MURMUR_SESSION_JOURNAL":           {&cfg.Logging.SessionJournal, "logging.session_journal"},
	}
	for name, entry := range boolVars {
		if value, ok := lookup(name); ok {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return configError(fmt.Sprintf("%s doit être un booléen", name))
			}
			*entry.target = parsed
			explicit[entry.key] = true
		}
	}

	if value, ok := lookup("MURMUR_STT_CHUNK_SECONDS"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return configError("MURMUR_STT_CHUNK_SECONDS doit être un entier")
		}
		cfg.STT.ChunkSeconds = parsed
	}
	if value, ok := lookup("MURMUR_STT_CHUNK_WORKERS"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return configError("MURMUR_STT_CHUNK_WORKERS doit être un entier")
		}
		cfg.STT.ChunkWorkers = parsed
	}
	if value, ok := lookup("MURMUR_STT_MAX_TEXT_BYTES"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return configError("MURMUR_STT_MAX_TEXT_BYTES doit être un entier")
		}
		cfg.STT.MaxTextBytes = parsed
	}
	if value, ok := lookup("MURMUR_STT_MAX_SEGMENTS"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return configError("MURMUR_STT_MAX_SEGMENTS doit être un entier")
		}
		cfg.STT.MaxSegments = parsed
	}
	if value, ok := lookup("MURMUR_PLUGIN_TIMEOUT_S"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return configError("MURMUR_PLUGIN_TIMEOUT_S doit être un entier")
		}
		cfg.Plugin.TimeoutS = parsed
	}
	intVars := map[string]struct {
		target *int
		key    string
	}{
		"MURMUR_OSD_SUCCESS_TIMEOUT_MS": {&cfg.UI.OSD.SuccessTimeoutMS, "ui.osd.success_timeout_ms"},
		"MURMUR_OSD_ERROR_TIMEOUT_MS":   {&cfg.UI.OSD.ErrorTimeoutMS, "ui.osd.error_timeout_ms"},
		"MURMUR_LOG_MAX_BYTES":          {&cfg.Logging.MaxBytes, "logging.max_bytes"},
		"MURMUR_LOG_MAX_FILES":          {&cfg.Logging.MaxFiles, "logging.max_files"},
		"MURMUR_SESSION_MAX_BYTES":      {&cfg.Logging.SessionMaxBytes, "logging.session_max_bytes"},
		"MURMUR_SESSION_MAX_FILES":      {&cfg.Logging.SessionMaxFiles, "logging.session_max_files"},
	}
	for name, entry := range intVars {
		if value, ok := lookup(name); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return configError(fmt.Sprintf("%s doit être un entier", name))
			}
			*entry.target = parsed
			explicit[entry.key] = true
		}
	}
	listVars := map[string]struct {
		target *[]string
		key    string
	}{
		"MURMUR_STT_ZONES":        {&cfg.Policy.STTZones, "policy.stt_zones"},
		"MURMUR_CLEANUP_ZONES":    {&cfg.Policy.CleanupZones, "policy.cleanup_zones"},
		"MURMUR_PLUGIN_ZONES":     {&cfg.Policy.PluginZones, "policy.plugin_zones"},
		"MURMUR_TRUSTED_NETWORKS": {&cfg.Policy.TrustedNetworks, "policy.trusted_networks"},
	}
	for name, entry := range listVars {
		if value, ok := lookup(name); ok {
			*entry.target = splitList(value)
			explicit[entry.key] = true
		}
	}
	return nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func applyOverrides(cfg *Config, o Overrides, explicit map[string]bool) {
	setString(o.Profile, &cfg.General.Profile)
	setString(o.CommandPack, &cfg.General.CommandPack)
	setBool(o.ASCIIQuotes, &cfg.General.ASCIIQuotes)
	setString(o.AudioSource, &cfg.Audio.Source)
	setBool(o.KeepRecordings, &cfg.Audio.KeepRecordings)
	setString(o.STTProvider, &cfg.STT.Provider)
	setString(o.STTFallback, &cfg.STT.Fallback)
	setInt(o.STTChunkSeconds, &cfg.STT.ChunkSeconds)
	setBool(o.STTLiveChunking, &cfg.STT.LiveChunking)
	setInt(o.STTChunkWorkers, &cfg.STT.ChunkWorkers)
	setInt(o.STTMaxTextBytes, &cfg.STT.MaxTextBytes)
	setInt(o.STTMaxSegments, &cfg.STT.MaxSegments)
	setBool(o.STTAllowPartial, &cfg.STT.AllowPartial)
	setBool(o.STTAllowMixed, &cfg.STT.AllowMixedProviders)
	setBool(o.CleanupEnabled, &cfg.Cleanup.Enabled)
	setString(o.CleanupProvider, &cfg.Cleanup.Provider)
	setBool(o.PluginEnabled, &cfg.Plugin.Enabled)
	setString(o.PluginMode, &cfg.Plugin.Mode)
	setInt(o.PluginTimeoutS, &cfg.Plugin.TimeoutS)
	setStrings(o.STTZones, &cfg.Policy.STTZones)
	setStrings(o.CleanupZones, &cfg.Policy.CleanupZones)
	setStrings(o.PluginZones, &cfg.Policy.PluginZones)
	setStrings(o.TrustedNetworks, &cfg.Policy.TrustedNetworks)
	setBool(o.RetainDiagnostics, &cfg.Policy.RetainDiagnostics)
	setString(o.SinkMode, &cfg.Sink.Mode)
	setString(o.SinkAutoMode, &cfg.Sink.AutoMode)
	setString(o.ClipboardMode, &cfg.Sink.ClipboardMode)
	setString(o.SinkFile, &cfg.Sink.File)
	setString(o.UIMode, &cfg.UI.Mode)
	setBool(o.UISound, &cfg.UI.Sound)
	setString(o.LoggingLevel, &cfg.Logging.Level)
	for key, defined := range map[string]bool{
		"general.profile":           o.Profile != nil,
		"audio.keep_recordings":     o.KeepRecordings != nil,
		"policy.stt_zones":          o.STTZones != nil,
		"policy.cleanup_zones":      o.CleanupZones != nil,
		"policy.plugin_zones":       o.PluginZones != nil,
		"policy.retain_diagnostics": o.RetainDiagnostics != nil,
		"sink.clipboard_mode":       o.ClipboardMode != nil,
	} {
		if defined {
			explicit[key] = true
		}
	}
}

var profileDefaultKeys = []string{
	"audio.keep_recordings",
	"policy.stt_zones",
	"policy.cleanup_zones",
	"policy.plugin_zones",
	"policy.retain_diagnostics",
	"sink.clipboard_mode",
}

type profileDefaults struct {
	sttZones          []string
	cleanupZones      []string
	pluginZones       []string
	keepRecordings    bool
	retainDiagnostics bool
	clipboardMode     string
}

var profiles = map[string]profileDefaults{
	"offline": {
		sttZones: []string{"local"}, cleanupZones: []string{"local"}, pluginZones: []string{"local"},
		clipboardMode: "clean-only",
	},
	"local": {
		sttZones: []string{"local"}, cleanupZones: []string{"local"}, pluginZones: []string{"local"},
		clipboardMode: "clean-only",
	},
	"lan": {
		sttZones: []string{"local", "lan"}, cleanupZones: []string{"local", "lan"}, pluginZones: []string{"local", "lan"},
		clipboardMode: "clean-only",
	},
	"lan-cloud-cleanup": {
		sttZones: []string{"local", "lan"}, cleanupZones: []string{"local", "lan", "internet"}, pluginZones: []string{"local", "lan", "internet"},
		clipboardMode: "clean-only",
	},
	"cloud": {
		sttZones: []string{"local", "lan", "internet"}, cleanupZones: []string{"local", "lan", "internet"}, pluginZones: []string{"local", "lan", "internet"},
		clipboardMode: "clean-only",
	},
	"cloud-no-history": {
		sttZones: []string{"local", "lan", "internet"}, cleanupZones: []string{"local", "lan", "internet"}, pluginZones: []string{"local", "lan", "internet"},
		clipboardMode: "none",
	},
}

func applyProfileDefaults(cfg *Config, explicit map[string]bool) {
	profile, ok := profiles[cfg.General.Profile]
	if !ok {
		return
	}
	if !explicit["policy.stt_zones"] {
		cfg.Policy.STTZones = append([]string(nil), profile.sttZones...)
	}
	if !explicit["policy.cleanup_zones"] {
		cfg.Policy.CleanupZones = append([]string(nil), profile.cleanupZones...)
	}
	if !explicit["policy.plugin_zones"] {
		cfg.Policy.PluginZones = append([]string(nil), profile.pluginZones...)
	}
	if !explicit["audio.keep_recordings"] {
		cfg.Audio.KeepRecordings = profile.keepRecordings
	}
	if !explicit["policy.retain_diagnostics"] {
		cfg.Policy.RetainDiagnostics = profile.retainDiagnostics
	}
	if !explicit["sink.clipboard_mode"] {
		cfg.Sink.ClipboardMode = profile.clipboardMode
	}
}

func resolveAutoSink(cfg *Config) {
	if cfg.Sink.Mode == "auto" {
		cfg.Sink.Mode = cfg.Sink.AutoMode
	}
}

func setString(value *string, target *string) {
	if value != nil {
		*target = *value
	}
}

func setBool(value *bool, target *bool) {
	if value != nil {
		*target = *value
	}
}

func setInt(value *int, target *int) {
	if value != nil {
		*target = *value
	}
}

func setStrings(value *[]string, target *[]string) {
	if value != nil {
		*target = append([]string(nil), (*value)...)
	}
}

func configError(message string) error {
	return errs.New(errs.ConfigInvalid, "config", message)
}
