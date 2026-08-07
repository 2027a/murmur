package session

import (
	"context"
	"io"
	"os"
	"strconv"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/audio/pwrecord"
	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/core"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/sink"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
	"git.2027a.net/mathieu/murmur/internal/textpipe"
	"git.2027a.net/mathieu/murmur/internal/ui"
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
		switch options.Config.UI.Mode {
		case "notify":
			userInterface = ui.NewDesktopNotifications(nil)
		case "osd":
			userInterface = ui.NewOSD(executor, ui.OSDOptions{
				Helper:            options.Config.UI.OSD.Helper,
				CloseTimeout:      time.Duration(options.Config.UI.OSD.SuccessTimeoutMS)*time.Millisecond + ui.OSDHelperExitGrace,
				ErrorCloseTimeout: time.Duration(options.Config.UI.OSD.ErrorTimeoutMS)*time.Millisecond + ui.OSDHelperExitGrace,
				Args: []string{
					"--position", options.Config.UI.OSD.Position,
					"--success-timeout-ms", strconv.Itoa(options.Config.UI.OSD.SuccessTimeoutMS),
					"--error-timeout-ms", strconv.Itoa(options.Config.UI.OSD.ErrorTimeoutMS),
				},
			})
		default:
			userInterface = ui.NewNone()
		}
	}
	if options.Config.UI.Sound {
		userInterface = ui.NewSound(userInterface, executor, "", nil)
	}
	recorder := options.Recorder
	if recorder == nil {
		recorder = pwrecord.New(executor)
	}
	return NewOrchestrator(OrchestratorOptions{
		Context: options.Context, SessionID: options.SessionID, RuntimeDir: options.RuntimeDir,
		Recorder: recorder, RecordOptions: audio.RecordOptions{
			Directory: options.RuntimeDir, Target: options.Config.Audio.Source,
			Retention: audio.RetentionOptions{Keep: options.Config.Audio.KeepRecordings || options.Config.Policy.RetainDiagnostics},
		},
		ChunkDuration: time.Duration(options.Config.STT.ChunkSeconds) * time.Second,
		LiveChunking:  options.Config.STT.LiveChunking,
		Transcriber:   chain, Processor: processor, Sink: delivery, UI: userInterface, Bus: options.Bus,
		Machine: machine, StartedAt: startedAt,
	})
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
