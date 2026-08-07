package cli

import (
	"bytes"
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

func (r commandResult) validate() error {
	if r.Version != 1 {
		return fmt.Errorf("version de résultat invalide: %d", r.Version)
	}
	if r.Command != "transcribe" && r.Command != "cleanup" && r.Command != "dictate" {
		return fmt.Errorf("commande de résultat invalide: %q", r.Command)
	}
	if r.Success != (r.Error == nil) {
		return errors.New("success et error sont incohérents")
	}
	if r.Segments == nil || r.Warnings == nil {
		return errors.New("segments et warnings doivent être des tableaux")
	}
	if r.SegmentCount != len(r.Segments) {
		return fmt.Errorf("segment_count=%d pour %d segments", r.SegmentCount, len(r.Segments))
	}
	if r.Command == "cleanup" && len(r.Segments) != 0 {
		return errors.New("cleanup ne peut pas contenir de segments STT")
	}
	if r.Command != "cleanup" && r.Success && len(r.Segments) == 0 {
		return errors.New("une transcription réussie doit contenir au moins un segment")
	}

	type metadata struct {
		model    string
		language string
	}
	providers := make(map[string]metadata)
	parts := make([]string, 0, len(r.Segments))
	previousIndex := -1
	for _, segment := range r.Segments {
		if segment.Index <= previousIndex {
			return fmt.Errorf("index de segment non strictement croissant: %d après %d", segment.Index, previousIndex)
		}
		previousIndex = segment.Index
		if segment.Provider == "" || segment.Provider == stt.ProviderMixed {
			return fmt.Errorf("provider de segment invalide à l'index %d", segment.Index)
		}
		text := strings.TrimSpace(segment.Transcript)
		if text == "" {
			return fmt.Errorf("transcript de segment vide à l'index %d", segment.Index)
		}
		parts = append(parts, text)
		current := metadata{model: segment.Model, language: segment.Language}
		if previous, ok := providers[segment.Provider]; ok && previous != current {
			return fmt.Errorf("métadonnées incohérentes pour le provider %q", segment.Provider)
		}
		providers[segment.Provider] = current
	}

	if len(r.Segments) == 0 {
		if r.Provider == stt.ProviderMixed {
			return errors.New("provider mixed sans segments")
		}
		return nil
	}
	if r.TranscriptRaw != strings.Join(parts, " ") {
		return errors.New("transcript_raw ne correspond pas aux segments")
	}
	if len(providers) > 1 {
		if r.Provider != stt.ProviderMixed || r.Model != "" || r.Language != "" {
			return errors.New("provenance globale incohérente pour des providers mixtes")
		}
		return nil
	}
	for provider, values := range providers {
		if r.Provider != provider || r.Model != values.model || r.Language != values.language {
			return errors.New("provenance globale incohérente avec les segments")
		}
	}
	return nil
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
	var input []byte
	stdin, cancelRead, err := app.cancellableStdin()
	if err == nil {
		input, err = readAllBoundedStdin(ctx, stdin, cancelRead, maxCleanupInputBytes+1)
		result.TranscriptRaw = string(input)
		result.TranscriptFinal = string(input)
		if err == nil && len(input) > maxCleanupInputBytes {
			err = errs.New(errs.Unavailable, "stdin", "entrée cleanup supérieure à 4 MiB")
		}
	} else {
		result.TranscriptRaw = ""
		result.TranscriptFinal = ""
	}
	if err != nil {
		if errs.Classify(err) == errs.Internal || errs.Classify(err) == errs.Cancelled {
			return finishCommand(result, err, *jsonOutput, stdout, stderr, emitter, true)
		}
		err = errs.Wrap(errs.Unavailable, "stdin", "lecture de stdin impossible", err)
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

type dictateOptions struct {
	sinkMode    string
	jsonOutput  bool
	eventOutput bool
}

func parseDictateOptions(args []string) (dictateOptions, error) {
	options := dictateOptions{sinkMode: "stdout"}
	flags := flag.NewFlagSet("dictate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.sinkMode, "sink", options.sinkMode, "sink stdout ou file")
	flags.BoolVar(&options.jsonOutput, "json", false, "sortie JSON")
	flags.BoolVar(&options.eventOutput, "events", false, "événements JSONL sur stderr")
	if err := flags.Parse(args); err != nil {
		return dictateOptions{}, err
	}
	if flags.NArg() != 0 || (options.sinkMode != "stdout" && options.sinkMode != "file") {
		return dictateOptions{}, errors.New("options dictate invalides")
	}
	return options, nil
}

func (app App) dictateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, parseErr := parseDictateOptions(args)
	if parseErr != nil {
		return usageError(stderr, "usage invalide pour dictate")
	}
	emitter := newCommandEmitter(options.eventOutput, stderr, "dictate")
	result := newCommandResult("dictate")
	emitter.emit(events.SessionStarted, "starting", "", "", "", 0, 0, "")
	none := "none"
	noSound := false
	cfg, err := app.resolve(config.Overrides{
		SinkMode: &options.sinkMode, ClipboardMode: &none, UIMode: &none, UISound: &noSound,
	})
	if err != nil {
		return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
	}
	setConfiguredSTTMetadata(&result, cfg)
	buildOptions := core.BuildOptions{BuildingProvider: func(provider string) {
		setProviderMetadata(&result, cfg, provider)
	}}
	transcriber, err := core.BuildTranscriber(ctx, cfg, buildOptions)
	if err != nil {
		return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
	}
	processor, err := core.BuildProcessor(ctx, cfg, buildOptions)
	if err != nil {
		return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
	}
	runtimeDir, err := app.runtime()
	if err != nil {
		return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
	}

	captureCtx, cancelCapture := context.WithCancel(context.Background())
	defer cancelCapture()
	recorder := pwrecord.New(sysexec.OSExecutor{
		Env: config.CredentialHelperEnvironment(cfg, os.Environ()),
	})
	recording, err := recorder.Start(captureCtx, audio.RecordOptions{
		Directory: runtimeDir, Target: cfg.Audio.Source,
		Retention: audio.RetentionOptions{Keep: cfg.Audio.KeepRecordings || cfg.Policy.RetainDiagnostics},
	})
	if err != nil {
		return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
	}
	captureReady := false
	select {
	case err = <-recording.Ready():
		if err != nil {
			_ = recording.Abort()
			return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
		}
		captureReady = true
	case <-ctx.Done():
	}
	if captureReady {
		emitter.emit(events.CaptureReady, "recording", "", "", "", 0, 0, "")
	}

	if err := ctx.Err(); err != nil {
		return finishCommand(result, errors.Join(
			errs.Wrap(errs.Cancelled, "stdin", "lecture de stdin annulée", err),
			recording.Abort(),
		), options.jsonOutput, stdout, stderr, emitter, false)
	}
	if err := app.waitForDictateStop(ctx, recording); err != nil {
		return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
	}
	if err := ctx.Err(); err != nil {
		return finishCommand(result, errors.Join(
			errs.Wrap(errs.Cancelled, "stdin", "lecture de stdin annulée", err),
			recording.Abort(),
		), options.jsonOutput, stdout, stderr, emitter, false)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	file, err := recording.Stop(stopCtx)
	cancelStop()
	if err != nil {
		return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
	}
	emitter.emit(events.StateChanged, "transcribing", "", "", "", 0, 0, "")
	result, err = executeTranscriptionWith(ctx, cfg, file.Path(), transcriber, processor, emitter, "dictate")
	if err == nil && !(options.jsonOutput && options.sinkMode == "stdout") {
		var delivery sink.Sink
		delivery, err = sink.New(cfg, sink.Options{Stdout: stdout})
		if err == nil {
			err = deliverDictate(delivery, result.TranscriptFinal)
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
	return finishCommand(result, err, options.jsonOutput, stdout, stderr, emitter, false)
}

func deliverDictate(delivery sink.Sink, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sink.ExternalDeliveryTimeout)
	defer cancel()
	return delivery.Deliver(ctx, text)
}

type stdinReadCanceler interface {
	io.Reader
	CancelRead() error
}

type stdinCancelFunc func() error

func (app App) waitForDictateStop(ctx context.Context, recording audio.Recording) error {
	input, cancelRead, err := app.dictateInput()
	if err != nil {
		return errors.Join(err, recording.Abort())
	}
	stdinDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, input)
		stdinDone <- err
	}()

	select {
	case <-ctx.Done():
		cancelErr := cancelRead()
		readErr := waitForStdinCleanup(stdinDone)
		abortErr := recording.Abort()
		return errors.Join(errs.Wrap(errs.Cancelled, "stdin", "lecture de stdin annulée", ctx.Err()), cancelErr, readErr, abortErr)
	case err := <-stdinDone:
		return err
	case <-recording.Done():
		cancelErr := cancelRead()
		readErr := waitForStdinCleanup(stdinDone)
		recordingErr := recording.Err()
		if errs.Classify(recordingErr) != errs.AudioFailed {
			recordingErr = errs.Wrap(errs.AudioFailed, "audio", "capture audio interrompue", recordingErr)
		}
		return errors.Join(recordingErr, cancelErr, readErr, recording.Abort())
	}
}

func waitForStdinCleanup(done <-chan error) error {
	return <-done
}

func (app App) dictateInput() (io.Reader, stdinCancelFunc, error) {
	input, cancelRead, err := app.cancellableStdin()
	if err != nil {
		return nil, nil, err
	}
	if cancelRead == nil {
		return input, func() error { return nil }, nil
	}
	return input, cancelRead, nil
}

func (app App) cancellableStdin() (io.Reader, stdinCancelFunc, error) {
	if app.Stdin == nil {
		return os.Stdin, os.Stdin.Close, nil
	}
	if input, ok := app.Stdin.(stdinReadCanceler); ok {
		return input, input.CancelRead, nil
	}
	if isKnownFiniteReader(app.Stdin) {
		return app.Stdin, nil, nil
	}
	return nil, nil, errs.New(errs.Internal, "stdin", "stdin emprunté doit implémenter CancelRead")
}

func isKnownFiniteReader(input io.Reader) bool {
	switch input.(type) {
	case *bytes.Buffer, *bytes.Reader, *strings.Reader:
		return true
	default:
		return false
	}
}

func readAllBoundedStdin(ctx context.Context, input io.Reader, cancelRead stdinCancelFunc, limit int64) ([]byte, error) {
	if cancelRead == nil {
		return io.ReadAll(io.LimitReader(input, limit))
	}
	type readResult struct {
		data []byte
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(input, limit))
		done <- readResult{data: data, err: err}
	}()
	select {
	case result := <-done:
		return result.data, result.err
	case <-ctx.Done():
		cancelErr := cancelRead()
		result := <-done
		return nil, errors.Join(
			errs.Wrap(errs.Cancelled, "stdin", "lecture de stdin annulée", ctx.Err()),
			cancelErr,
			result.err,
		)
	}
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
	result.Model, result.Language = "", ""
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
	emitter.emit(events.ProviderSelected, "transcribing", result.Provider, "", "", 0, 0, "")
	emitter.emit(events.StateChanged, "processing", result.Provider, "", "", 0, 0, "")
	processed, err := processor.Run(ctx, multi.Text)
	if err != nil {
		return result, err
	}
	result.TranscriptFinal = processed.Text
	for _, warning := range processed.Warnings {
		result.Warnings = append(result.Warnings, commandWarning{
			Class: warning.Class, Provider: result.Provider, Component: warning.Component, Message: warning.Message,
		})
		emitter.emit(events.Degraded, "processing", result.Provider, warning.Class, warning.Component, 0, 0, "")
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
		terminal := "failed"
		if result.Error.Class == errs.Cancelled {
			terminal = "cancelled"
		}
		emitter.emit(events.Terminal, "idle", result.Provider, result.Error.Class, result.Error.Component, 0, 0, terminal)
	} else {
		result.Success = true
		emitter.emit(events.Terminal, "idle", result.Provider, "", "", 0, 0, "completed")
	}
	finishErr := runErr
	if finishErr == nil {
		if err := emitter.failure(); err != nil {
			finishErr = errs.Wrap(errs.Internal, "io", "écriture du flux d'événements impossible", err)
			result.Success = false
			result.Error = safeError(finishErr)
		}
	}
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			if !emitter.enabled {
				fmt.Fprintln(stderr, safeError(err).Message)
			}
			return 1
		}
	} else if finishErr != nil {
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
	if finishErr != nil {
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
	err       error
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
	if e.err != nil {
		return
	}
	e.err = events.EncodeJSONL(shortWriteCheckingWriter{e.writer}, events.Event{
		Version: 1, Type: kind, Timestamp: time.Now().UTC(), SessionID: e.sessionID,
		State: state, Result: result, Provider: provider, ErrorClass: class,
		Component: component, Partial: partial, Total: total,
	})
}

func (e *commandEmitter) failure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

type shortWriteCheckingWriter struct {
	io.Writer
}

func (w shortWriteCheckingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}
