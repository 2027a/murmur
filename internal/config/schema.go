package config

type Config struct {
	General GeneralConfig `toml:"general"`
	Audio   AudioConfig   `toml:"audio"`
	STT     STTConfig     `toml:"stt"`
	Cleanup CleanupConfig `toml:"cleanup"`
	Plugin  PluginConfig  `toml:"plugin"`
	Policy  PolicyConfig  `toml:"policy"`
	Sink    SinkConfig    `toml:"sink"`
	UI      UIConfig      `toml:"ui"`
	Logging LoggingConfig `toml:"logging"`
}

type GeneralConfig struct {
	Profile     string `toml:"profile"`
	CommandPack string `toml:"command_pack"`
	ASCIIQuotes bool   `toml:"ascii_quotes"`
}

type AudioConfig struct {
	Source         string `toml:"source"`
	KeepRecordings bool   `toml:"keep_recordings"`
}

type STTConfig struct {
	Provider            string                    `toml:"provider"`
	Fallback            string                    `toml:"fallback"`
	ChunkSeconds        int                       `toml:"chunk_seconds"`
	ChunkPauseMS        int                       `toml:"chunk_pause_ms"`
	ChunkOverlapMS      int                       `toml:"chunk_overlap_ms"`
	LiveChunking        bool                      `toml:"live_chunking"`
	ChunkWorkers        int                       `toml:"chunk_workers"`
	MaxTextBytes        int                       `toml:"max_text_bytes"`
	MaxSegments         int                       `toml:"max_segments"`
	AllowPartial        bool                      `toml:"allow_partial"`
	AllowMixedProviders bool                      `toml:"allow_mixed_providers"`
	Providers           map[string]ProviderConfig `toml:"providers"`
}

type ProviderConfig struct {
	Type           string   `toml:"type"`
	BaseURL        string   `toml:"base_url"`
	Model          string   `toml:"model"`
	Language       string   `toml:"language"`
	Prompt         string   `toml:"prompt"`
	TimeoutS       int      `toml:"timeout_s"`
	Zone           string   `toml:"zone"`
	AllowInsecure  bool     `toml:"allow_insecure"`
	HealthEndpoint string   `toml:"health_endpoint"`
	HealthEnabled  bool     `toml:"health_enabled"`
	Retries        int      `toml:"retries"`
	RetryBackoffMS int      `toml:"retry_backoff_ms"`
	APIKeyEnv      string   `toml:"api_key_env"`
	APIKeyFile     string   `toml:"api_key_file"`
	APIKeyCommand  []string `toml:"api_key_command"`
}

type CredentialConfig struct {
	APIKeyEnv     string   `toml:"api_key_env"`
	APIKeyFile    string   `toml:"api_key_file"`
	APIKeyCommand []string `toml:"api_key_command"`
}

type CleanupConfig struct {
	Enabled   bool                             `toml:"enabled"`
	Provider  string                           `toml:"provider"`
	Prompt    string                           `toml:"prompt"`
	Providers map[string]CleanupProviderConfig `toml:"providers"`
}

type CleanupProviderConfig struct {
	Type            string   `toml:"type"`
	BaseURL         string   `toml:"base_url"`
	Model           string   `toml:"model"`
	ReasoningEffort string   `toml:"reasoning_effort"`
	TimeoutS        int      `toml:"timeout_s"`
	Zone            string   `toml:"zone"`
	AllowInsecure   bool     `toml:"allow_insecure"`
	APIKeyEnv       string   `toml:"api_key_env"`
	APIKeyFile      string   `toml:"api_key_file"`
	APIKeyCommand   []string `toml:"api_key_command"`
}

func (p ProviderConfig) credentials() CredentialConfig {
	return CredentialConfig{APIKeyEnv: p.APIKeyEnv, APIKeyFile: p.APIKeyFile, APIKeyCommand: p.APIKeyCommand}
}

func (p CleanupProviderConfig) credentials() CredentialConfig {
	return CredentialConfig{APIKeyEnv: p.APIKeyEnv, APIKeyFile: p.APIKeyFile, APIKeyCommand: p.APIKeyCommand}
}

func (p ProviderConfig) Credentials() CredentialConfig        { return p.credentials() }
func (p CleanupProviderConfig) Credentials() CredentialConfig { return p.credentials() }

type PluginConfig struct {
	Enabled  bool   `toml:"enabled"`
	Mode     string `toml:"mode"`
	TimeoutS int    `toml:"timeout_s"`
}

type PolicyConfig struct {
	STTZones          []string `toml:"stt_zones"`
	CleanupZones      []string `toml:"cleanup_zones"`
	PluginZones       []string `toml:"plugin_zones"`
	TrustedNetworks   []string `toml:"trusted_networks"`
	RetainDiagnostics bool     `toml:"retain_diagnostics"`
}

type SinkConfig struct {
	Mode           string   `toml:"mode"`
	AutoMode       string   `toml:"auto_mode"`
	ClipboardMode  string   `toml:"clipboard_mode"`
	PasteShortcuts []string `toml:"paste_shortcuts"`
	TypeNewlines   string   `toml:"type_newlines"`
	File           string   `toml:"file"`
}

type UIConfig struct {
	Mode  string    `toml:"mode"`
	Sound bool      `toml:"sound"`
	OSD   OSDConfig `toml:"osd"`
}

type OSDConfig struct {
	Helper           string `toml:"helper"`
	Position         string `toml:"position"`
	SuccessTimeoutMS int    `toml:"success_timeout_ms"`
	ErrorTimeoutMS   int    `toml:"error_timeout_ms"`
}

type LoggingConfig struct {
	Level           string `toml:"level"`
	MaxBytes        int    `toml:"max_bytes"`
	MaxFiles        int    `toml:"max_files"`
	SessionJournal  bool   `toml:"session_journal"`
	SessionMaxBytes int    `toml:"session_max_bytes"`
	SessionMaxFiles int    `toml:"session_max_files"`
}

func Defaults() Config {
	return Config{
		General: GeneralConfig{Profile: "local", CommandPack: "auto"},
		Audio:   AudioConfig{},
		STT: STTConfig{
			Provider:     "local",
			ChunkSeconds: 5,
			ChunkPauseMS: 500,
			LiveChunking: true,
			ChunkWorkers: 2,
			MaxTextBytes: 16 << 20,
			MaxSegments:  4096,
			Providers: map[string]ProviderConfig{
				"local": {
					Type:     "openai-compatible",
					BaseURL:  "http://127.0.0.1:8000/v1",
					Model:    "large-v3",
					Language: "fr",
					TimeoutS: 120,
					Zone:     "local",
				},
			},
		},
		Cleanup: CleanupConfig{Providers: make(map[string]CleanupProviderConfig)},
		Plugin:  PluginConfig{Mode: "plain", TimeoutS: 2},
		Policy: PolicyConfig{
			STTZones:     []string{"local"},
			CleanupZones: []string{"local"},
			PluginZones:  []string{"local"},
		},
		Sink: SinkConfig{
			Mode:           "paste",
			AutoMode:       "paste",
			ClipboardMode:  "clean-only",
			PasteShortcuts: []string{"ctrl+shift+v", "shift+insert", "ctrl+v"},
			TypeNewlines:   "space",
		},
		UI: UIConfig{
			Mode: "notify",
			OSD: OSDConfig{
				Helper: "murmur-osd", Position: "top-center",
				SuccessTimeoutMS: 2500, ErrorTimeoutMS: 4000,
			},
		},
		Logging: LoggingConfig{
			Level: "info", MaxBytes: 1 << 20, MaxFiles: 3, SessionJournal: true,
			SessionMaxBytes: 1 << 20, SessionMaxFiles: 3,
		},
	}
}

type Overrides struct {
	Profile           *string
	CommandPack       *string
	ASCIIQuotes       *bool
	AudioSource       *string
	KeepRecordings    *bool
	STTProvider       *string
	STTFallback       *string
	STTChunkSeconds   *int
	STTLiveChunking   *bool
	STTChunkWorkers   *int
	STTMaxTextBytes   *int
	STTMaxSegments    *int
	STTAllowPartial   *bool
	STTAllowMixed     *bool
	CleanupEnabled    *bool
	CleanupProvider   *string
	PluginEnabled     *bool
	PluginMode        *string
	PluginTimeoutS    *int
	STTZones          *[]string
	CleanupZones      *[]string
	PluginZones       *[]string
	TrustedNetworks   *[]string
	RetainDiagnostics *bool
	SinkMode          *string
	SinkAutoMode      *string
	ClipboardMode     *string
	SinkFile          *string
	UIMode            *string
	UISound           *bool
	LoggingLevel      *string
}
