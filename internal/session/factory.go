package session

import (
	"context"
	"io"
	"os"
	"strconv"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/pwrecord"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/core"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/sink"
	"git.2027a.net/2027a/murmur/internal/sysexec"
	"git.2027a.net/2027a/murmur/internal/textpipe"
	"git.2027a.net/2027a/murmur/internal/ui"
)

type BuildOptions struct {
	Context        context.Context
	Config         config.Config
	RuntimeDir     string
	SessionID      string
	ClipboardOnly  bool
	Stdout         io.Writer
	Executor       sysexec.Executor
	SecretExecutor sysexec.Executor
	Bus            *events.Bus
	UI             ui.UI
	Recorder       audio.Recorder
	Bootstrap      *Bootstrap
}

func buildPlugin(cfg config.Config) (textpipe.Plugin, error) {
	return core.BuildPlugin(cfg)
}

func Build(options BuildOptions) (*Orchestrator, error) {
	if err := config.Validate(options.Config); err != nil {
		return nil, err
	}
	if err := config.ValidateClipboardOnly(options.Config, options.ClipboardOnly); err != nil {
		return nil, err
	}
	if options.SessionID == "" || options.RuntimeDir == "" {
		return nil, errs.New(errs.ConfigInvalid, "session", "identifiant ou répertoire de session manquant")
	}
	var machine *Machine
	var startedAt time.Time
	if options.Bootstrap != nil {
		options.Context = options.Bootstrap.Context()
		machine, startedAt = options.Bootstrap.machineOptions()
	}
	if options.Context == nil {
		options.Context = context.Background()
	}
	executor, secretExecutor := processExecutors(options.Config, options.Executor, options.SecretExecutor)
	buildOptions := core.BuildOptions{Executor: secretExecutor}
	if options.Bootstrap != nil {
		buildOptions.BuildingProvider = options.Bootstrap.setBuildingProvider
	}
	chain, err := core.BuildTranscriber(options.Context, options.Config, buildOptions)
	if err != nil {
		return nil, err
	}
	processor, err := core.BuildProcessor(options.Context, options.Config, buildOptions)
	if err != nil {
		return nil, err
	}
	delivery, err := sink.New(options.Config, sink.Options{
		Executor: executor, Stdout: options.Stdout, ClipboardOnly: options.ClipboardOnly,
	})
	if err != nil {
		return nil, err
	}
	userInterface := options.UI
	if userInterface == nil {
		userInterface = BuildUI(options.Config, options.RuntimeDir, options.ClipboardOnly, executor)
	} else if options.Config.UI.Sound {
		if _, wrapped := userInterface.(*ui.Sound); !wrapped {
			userInterface = ui.NewSound(userInterface, executor, "", nil)
		}
	}
	var meter func(wav.Level)
	if options.Config.UI.Mode == "auto" || options.Config.UI.Mode == "quickshell" {
		if m, ok := userInterface.(interface{ Meter(wav.Level) }); ok {
			meter = m.Meter
		}
	}
	recorder := options.Recorder
	if recorder == nil {
		recorder = pwrecord.New(executor)
	}
	return NewOrchestrator(OrchestratorOptions{
		Context: options.Context, SessionID: options.SessionID, RuntimeDir: options.RuntimeDir,
		Recorder: recorder, RecordOptions: audio.RecordOptions{
			Directory: options.RuntimeDir, Target: options.Config.Audio.Source,
			Meter:     meter,
			Retention: audio.RetentionOptions{Keep: options.Config.Audio.KeepRecordings || options.Config.Policy.RetainDiagnostics},
		},
		ChunkDuration:    time.Duration(options.Config.STT.ChunkSeconds) * time.Second,
		ChunkPauseWindow: time.Duration(options.Config.STT.ChunkPauseMS) * time.Millisecond,
		ChunkOverlap:     time.Duration(options.Config.STT.ChunkOverlapMS) * time.Millisecond,
		MaxSegments:      options.Config.STT.MaxSegments,
		LiveChunking:     options.Config.STT.LiveChunking,
		Transcriber:      chain, Processor: processor, Sink: delivery, UI: userInterface, Bus: options.Bus,
		Machine: machine, StartedAt: startedAt,
	})
}

// BuildUI also serves bootstrap so slow credentials/provider setup is visible.
func BuildUI(cfg config.Config, runtimeDir string, clipboardOnly bool, executor sysexec.Executor) ui.UI {
	executor, _ = processExecutors(cfg, executor, nil)
	var userInterface ui.UI
	switch cfg.UI.Mode {
	case "auto", "quickshell":
		var fallback ui.UI
		if cfg.UI.Mode == "auto" {
			fallback = ui.NewDesktopNotifications(nil)
		}
		delivery := cfg.Sink.Mode
		if clipboardOnly {
			delivery = "clipboard"
		}
		userInterface = ui.NewBar(ui.BarSocket(runtimeDir), delivery, cfg.Cleanup.Enabled, fallback, nil)
	case "notify":
		userInterface = ui.NewDesktopNotifications(nil)
	case "osd":
		closeTimeout := time.Duration(max(cfg.UI.OSD.SuccessTimeoutMS, cfg.UI.OSD.ErrorTimeoutMS))*time.Millisecond + ui.OSDHelperExitGrace
		userInterface = ui.NewOSD(executor, ui.OSDOptions{
			Helper: cfg.UI.OSD.Helper, CloseTimeout: closeTimeout,
			Args: []string{"--position", cfg.UI.OSD.Position, "--success-timeout-ms", strconv.Itoa(cfg.UI.OSD.SuccessTimeoutMS), "--error-timeout-ms", strconv.Itoa(cfg.UI.OSD.ErrorTimeoutMS)},
		})
	default:
		userInterface = ui.NewNone()
	}
	if cfg.UI.Sound {
		userInterface = ui.NewSound(userInterface, executor, "", nil)
	}
	return userInterface
}

func processExecutors(cfg config.Config, executor, secretExecutor sysexec.Executor) (sysexec.Executor, sysexec.Executor) {
	environment := os.Environ()
	filteredEnvironment := config.CredentialHelperEnvironment(cfg, environment)
	if executor == nil {
		executor = sysexec.OSExecutor{Env: filteredEnvironment}
	}
	if secretExecutor == nil {
		secretExecutor = sysexec.OSExecutor{Env: filteredEnvironment}
	}
	return executor, secretExecutor
}
