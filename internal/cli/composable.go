package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/audio/pwrecord"
	"git.2027a.net/mathieu/murmur/internal/cleanup"
	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/core"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/sink"
	"git.2027a.net/mathieu/murmur/internal/stt"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
	"git.2027a.net/mathieu/murmur/internal/textpipe"
)

type commandError struct {
	Class     errs.Class `json:"class"`
	Component string     `json:"component"`
	Message   string     `json:"message"`
}

type commandWarning struct {
	Class     errs.Class `json:"class"`
	Provider  string     `json:"provider"`
	Component string     `json:"component"`
	Message   string     `json:"message"`
}

type commandSegment struct {
	Index      int    `json:"index"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Language   string `json:"language"`
	Transcript string `json:"transcript"`
}

const maxCleanupInputBytes = 4 << 20

// commandResult is the stable v1 result shared by composable commands. Fields
// are intentionally not omitted so consumers never have to infer a shape.
type commandResult struct {
	Version         int              `json:"v"`
	Command         string           `json:"command"`
	Success         bool             `json:"success"`
	Provider        string           `json:"provider"`
	Model           string           `json:"model"`
	Language        string           `json:"language"`
	TranscriptRaw   string           `json:"transcript_raw"`
	TranscriptFinal string           `json:"transcript_final"`
	SegmentCount    int              `json:"segment_count"`
	Segments        []commandSegment `json:"segments"`
	Warnings        []commandWarning `json:"warnings"`
	Error           *commandError    `json:"error"`
}

func newCommandResult(command string) commandResult {
	return commandResult{Version: 1, Command: command, Segments: []commandSegment{}, Warnings: []commandWarning{}}
}

func (app App) transcribeCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("transcribe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "sortie JSON")
	eventOutput := flags.Bool("events", false, "événements JSONL sur stderr")
	file, err := parseFileFlags(flags, args)
	if err != nil {
		return usageError(stderr, "usage invalide pour transcribe")
	}
	emitter := newCommandEmitter(*eventOutput, stderr, "transcribe")
	emitter.emit(events.SessionStarted, "transcribing", "", "", "", 0, 0, "")
	result := newCommandResult("transcribe")

	cfg, err := app.resolve(config.Overrides{})
	if err == nil {
		result, err = executeTranscription(ctx, cfg, file, emitter)
	}
	return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, true)
}

func (app App) cleanupCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "sortie JSON")
	eventOutput := flags.Bool("events", false, "événements JSONL sur stderr")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return usageError(stderr, "usage invalide pour cleanup")
	}
	emitter := newCommandEmitter(*eventOutput, stderr, "cleanup")
	emitter.emit(events.SessionStarted, "processing", "", "", "", 0, 0, "")
	result := newCommandResult("cleanup")
	input, err := io.ReadAll(io.LimitReader(app.stdin(), maxCleanupInputBytes+1))
	result.TranscriptRaw = string(input)
	result.TranscriptFinal = string(input)
	if err != nil {
		err = errs.Wrap(errs.Unavailable, "stdin", "lecture de stdin impossible", err)
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, true)
	}
	if len(input) > maxCleanupInputBytes {
		err = errs.New(errs.Unavailable, "stdin", "entrée cleanup supérieure à 4 MiB")
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, true)
	}

	cfg, err := app.resolve(config.Overrides{})
	if err == nil {
		provider := cfg.Cleanup.Providers[cfg.Cleanup.Provider]
		result.Provider, result.Model, result.Language = cfg.Cleanup.Provider, provider.Model, ""
		var client *cleanup.Client
		client, err = core.BuildCleanup(ctx, cfg, core.BuildOptions{})
		if err == nil && client == nil {
			result.Provider, result.Model, result.Language = "", "", ""
			result.Warnings = append(result.Warnings, commandWarning{
				Component: "cleanup", Message: "cleanup désactivé: entrée conservée sans modification",
			})
		} else if err == nil {
			emitter.emit(events.ProviderSelected, "processing", result.Provider, "", "", 0, 0, "")
			cleaned := client.Clean(ctx, string(input))
			result.TranscriptFinal = cleaned.Text
			if cleaned.Degraded != nil {
				detail := safeError(cleaned.Degraded)
				result.Warnings = append(result.Warnings, commandWarning{
					Class: detail.Class, Provider: result.Provider, Component: "cleanup", Message: cleanup.WarningDegraded,
				})
				emitter.emit(events.Degraded, "processing", result.Provider, detail.Class, "cleanup", 0, 0, "")
			}
		}
	}
	return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, true)
}

func (app App) dictateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dictate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sinkMode := flags.String("sink", "stdout", "sink stdout ou file")
	jsonOutput := flags.Bool("json", false, "sortie JSON")
	eventOutput := flags.Bool("events", false, "événements JSONL sur stderr")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || (*sinkMode != "stdout" && *sinkMode != "file") {
		return usageError(stderr, "usage invalide pour dictate")
	}
	emitter := newCommandEmitter(*eventOutput, stderr, "dictate")
	result := newCommandResult("dictate")
	emitter.emit(events.SessionStarted, "starting", "", "", "", 0, 0, "")
	none := "none"
	noSound := false
	cfg, err := app.resolve(config.Overrides{
		SinkMode: sinkMode, ClipboardMode: &none, UIMode: &none, UISound: &noSound,
	})
	if err != nil {
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
	}
	setConfiguredSTTMetadata(&result, cfg)
	buildOptions := core.BuildOptions{BuildingProvider: func(provider string) {
		setProviderMetadata(&result, cfg, provider)
	}}
	transcriber, err := core.BuildTranscriber(ctx, cfg, buildOptions)
	if err != nil {
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
	}
	processor, err := core.BuildProcessor(ctx, cfg, buildOptions)
	if err != nil {
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
	}
	runtimeDir, err := app.runtime()
	if err != nil {
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
	}

	captureCtx, cancelCapture := context.WithCancel(context.Background())
	defer cancelCapture()
	recorder := pwrecord.New(sysexec.OSExecutor{
		Env: sysexec.FilterEnvironment(os.Environ(), config.APIKeyEnvironmentNames(cfg)),
	})
	recording, err := recorder.Start(captureCtx, audio.RecordOptions{
		Directory: runtimeDir, Target: cfg.Audio.Source,
		Retention: audio.RetentionOptions{Keep: cfg.Audio.KeepRecordings || cfg.Policy.RetainDiagnostics},
	})
	if err != nil {
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
	}
	captureReady := false
	select {
	case err = <-recording.Ready():
		if err != nil {
			_ = recording.Abort()
			return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
		}
		captureReady = true
	case <-ctx.Done():
	}
	if captureReady {
		emitter.emit(events.CaptureReady, "recording", "", "", "", 0, 0, "")
	}

	if ctx.Err() == nil {
		stdinDone := make(chan struct{}, 1)
		go func() {
			_, _ = io.Copy(io.Discard, app.stdin())
			stdinDone <- struct{}{}
		}()
		select {
		case <-ctx.Done():
		case <-stdinDone:
		}
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	file, err := recording.Stop(stopCtx)
	cancelStop()
	if err != nil {
		return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
	}
	emitter.emit(events.StateChanged, "transcribing", "", "", "", 0, 0, "")
	result, err = executeTranscriptionWith(context.Background(), cfg, file.Path(), transcriber, processor, emitter, "dictate")
	if err == nil && !(*jsonOutput && *sinkMode == "stdout") {
		var delivery sink.Sink
		delivery, err = sink.New(cfg, sink.Options{Stdout: stdout})
		if err == nil {
			err = delivery.Deliver(context.Background(), result.TranscriptFinal)
		}
		if err == nil {
			emitter.emit(events.Delivered, "delivering", result.Provider, "", "", 0, 0, "")
		}
	}
	_, releaseErr := file.Release()
	if releaseErr != nil {
		if err != nil {
			err = errors.Join(err, releaseErr)
		} else {
			result.Warnings = append(result.Warnings, commandWarning{
				Class: errs.AudioFailed, Provider: result.Provider, Component: "audio", Message: "nettoyage audio dégradé",
			})
			emitter.emit(events.Degraded, "delivering", result.Provider, errs.AudioFailed, "audio", 0, 0, "")
		}
	}
	return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, false)
}

func executeTranscription(ctx context.Context, cfg config.Config, path string, emitter *commandEmitter) (commandResult, error) {
	result := newCommandResult("transcribe")
	setConfiguredSTTMetadata(&result, cfg)
	buildOptions := core.BuildOptions{BuildingProvider: func(provider string) {
		setProviderMetadata(&result, cfg, provider)
	}}
	transcriber, err := core.BuildTranscriber(ctx, cfg, buildOptions)
	if err != nil {
		return result, err
	}
	processor, err := core.BuildProcessor(ctx, cfg, buildOptions)
	if err != nil {
		return result, err
	}
	return executeTranscriptionWith(ctx, cfg, path, transcriber, processor, emitter, "transcribe")
}

func executeTranscriptionWith(ctx context.Context, cfg config.Config, path string, transcriber *stt.Chain, processor *textpipe.Pipeline, emitter *commandEmitter, command string) (commandResult, error) {
	result := newCommandResult(command)
	setConfiguredSTTMetadata(&result, cfg)
	multi, err := core.TranscribeFile(ctx, path, time.Duration(cfg.STT.ChunkSeconds)*time.Second, transcriber, stt.Progress{
		Started: func(index int, provider string) {
			emitter.emit(events.PartialStarted, "transcribing", provider, "", "stt", index+1, 0, "")
		},
		Done: func(index int, provider string) {
			emitter.emit(events.PartialDone, "transcribing", provider, "", "stt", index+1, 0, "")
		},
		Failed: func(index int, provider string, class errs.Class) {
			emitter.emit(events.PartialFailed, "transcribing", provider, class, "stt", index+1, 0, "")
		},
	})
	if err != nil {
		if provider := stt.ProviderFromError(err); provider != "" {
			setProviderMetadata(&result, cfg, provider)
		}
		return result, err
	}
	result.Provider = multi.Provider
	if provider, ok := cfg.STT.Providers[result.Provider]; ok {
		result.Model, result.Language = provider.Model, provider.Language
	}
	result.TranscriptRaw = multi.Text
	result.SegmentCount = len(multi.Segments)
	for _, segment := range multi.Segments {
		provider := cfg.STT.Providers[segment.Provider]
		result.Segments = append(result.Segments, commandSegment{
			Index: segment.Index, Provider: segment.Provider, Model: provider.Model,
			Language: provider.Language, Transcript: segment.Text,
		})
	}
	for _, warning := range multi.Warnings {
		result.Warnings = append(result.Warnings, commandWarning{
			Class: warning.Class, Provider: warning.Provider, Component: warning.Component, Message: warning.Message,
		})
		emitter.emit(events.Degraded, "transcribing", warning.Provider, warning.Class, warning.Component, 0, 0, "")
	}
	emitter.emit(events.ProviderSelected, "transcribing", result.Provider, "", "stt", 0, 0, "")
	emitter.emit(events.StateChanged, "processing", result.Provider, "", "", 0, 0, "")
	processed, err := processor.Run(ctx, multi.Text)
	if err != nil {
		return result, err
	}
	result.TranscriptFinal = processed.Text
	for _, warning := range processed.Warnings {
		result.Warnings = append(result.Warnings, commandWarning{
			Class: errs.Internal, Provider: result.Provider, Component: warning.Stage, Message: warning.Message,
		})
		emitter.emit(events.Degraded, "processing", result.Provider, errs.Internal, warning.Stage, 0, 0, "")
	}
	if strings.TrimSpace(result.TranscriptFinal) == "" {
		return result, errs.New(errs.NoSpeech, "text-pipeline", "le pipeline n'a produit aucun texte")
	}
	return result, nil
}

func parseFileFlags(flags *flag.FlagSet, args []string) (string, error) {
	var flagArgs, positional []string
	afterSeparator := false
	for _, arg := range args {
		if arg == "--" && !afterSeparator {
			afterSeparator = true
			continue
		}
		if !afterSeparator && strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
		} else {
			positional = append(positional, arg)
		}
	}
	if err := flags.Parse(flagArgs); err != nil || flags.NArg() != 0 || len(positional) != 1 {
		return "", errors.New("arguments invalides")
	}
	return positional[0], nil
}

func finishCommand(result commandResult, runErr error, jsonOutput bool, stdout, stderr io.Writer, emitter *commandEmitter, writePlain bool) int {
	if runErr != nil {
		result.Success = false
		result.Error = safeError(runErr)
		if provider := stt.ProviderFromError(runErr); provider != "" {
			result.Provider = provider
		}
		emitter.emit(events.Terminal, "", result.Provider, result.Error.Class, result.Error.Component, 0, 0, "failed")
	} else {
		result.Success = true
		emitter.emit(events.Terminal, "", result.Provider, "", "", 0, 0, "completed")
	}
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			if !emitter.enabled {
				fmt.Fprintln(stderr, safeError(err).Message)
			}
			return 1
		}
	} else if runErr != nil {
		if !emitter.enabled {
			fmt.Fprintln(stderr, result.Error.Message)
		}
	} else if writePlain {
		if _, err := io.WriteString(stdout, result.TranscriptFinal); err != nil {
			if !emitter.enabled {
				fmt.Fprintln(stderr, "écriture sur stdout impossible")
			}
			return 1
		}
		if result.Command == "transcribe" && !strings.HasSuffix(result.TranscriptFinal, "\n") {
			if _, err := io.WriteString(stdout, "\n"); err != nil {
				return 1
			}
		}
	}
	if runErr != nil {
		return 1
	}
	return 0
}

func setConfiguredSTTMetadata(result *commandResult, cfg config.Config) {
	setProviderMetadata(result, cfg, cfg.STT.Provider)
}

func setProviderMetadata(result *commandResult, cfg config.Config, name string) {
	if provider, ok := cfg.STT.Providers[name]; ok {
		result.Provider, result.Model, result.Language = name, provider.Model, provider.Language
		return
	}
	provider := cfg.Cleanup.Providers[name]
	result.Provider, result.Model, result.Language = name, provider.Model, ""
}

func safeError(err error) *commandError {
	detail := &commandError{Class: errs.Classify(err), Component: "internal", Message: "échec interne"}
	var typed *errs.Error
	if errors.As(err, &typed) {
		detail.Component = typed.Component
		detail.Message = typed.Msg
		return detail
	}
	switch detail.Class {
	case errs.Cancelled:
		detail.Component, detail.Message = "context", "opération annulée"
	case errs.Timeout:
		detail.Component, detail.Message = "context", "délai dépassé"
	}
	return detail
}

type commandEmitter struct {
	enabled   bool
	writer    io.Writer
	sessionID string
	mu        sync.Mutex
}

func newCommandEmitter(enabled bool, writer io.Writer, command string) *commandEmitter {
	return &commandEmitter{enabled: enabled, writer: writer, sessionID: command + "-" + newSessionID()}
}

func (e *commandEmitter) emit(kind events.Type, state, provider string, class errs.Class, component string, partial, total int, result string) {
	if !e.enabled {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = events.EncodeJSONL(e.writer, events.Event{
		Version: 1, Type: kind, Timestamp: time.Now().UTC(), SessionID: e.sessionID,
		State: state, Result: result, Provider: provider, ErrorClass: class,
		Component: component, Partial: partial, Total: total,
	})
}
