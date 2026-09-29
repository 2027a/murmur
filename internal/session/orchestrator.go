package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/ipc"
	"git.2027a.net/2027a/murmur/internal/sink"
	"git.2027a.net/2027a/murmur/internal/stt"
	"git.2027a.net/2027a/murmur/internal/textpipe"
	"git.2027a.net/2027a/murmur/internal/ui"
)

type Transcriber interface {
	TranscribeSegments(context.Context, []stt.Segment, stt.Progress) (stt.MultiResult, error)
}

type LiveTranscriber interface {
	TranscribeStream(context.Context, <-chan stt.Segment, stt.Progress) (stt.MultiResult, error)
}

type Processor interface {
	Run(context.Context, string) (textpipe.Result, error)
}

type OrchestratorOptions struct {
	Context          context.Context
	SessionID        string
	RuntimeDir       string
	Recorder         audio.Recorder
	RecordOptions    audio.RecordOptions
	ChunkDuration    time.Duration
	ChunkPauseWindow time.Duration
	ChunkOverlap     time.Duration
	MaxSegments      int
	LiveChunking     bool
	Transcriber      Transcriber
	Processor        Processor
	Sink             sink.Sink
	UI               ui.UI
	Bus              *events.Bus
	Now              func() time.Time
	Machine          *Machine
	StartedAt        time.Time
}

type Orchestrator struct {
	machine     *Machine
	runtimeDir  string
	recorder    audio.Recorder
	recordOpts  audio.RecordOptions
	chunk       time.Duration
	pauseWindow time.Duration
	overlap     time.Duration
	maxSegments int
	live        bool
	transcriber Transcriber
	processor   Processor
	sink        sink.Sink
	ui          ui.UI
	bus         *events.Bus
	now         func() time.Time
	startedAt   time.Time
	runStarted  time.Time
	parentCtx   context.Context

	ctx        context.Context
	cancelOps  context.CancelFunc
	stop       chan struct{}
	stopOnce   sync.Once
	beginOnce  sync.Once
	beginErr   error
	runOnce    sync.Once
	runDone    chan struct{}
	metadataMu sync.Mutex
	runResult  LastResult
	runErr     error
	persistErr error
	stopAt     time.Time
}

type eventPublisher struct {
	bus *events.Bus
	ui  ui.UI
}

func (p eventPublisher) Publish(event events.Event) {
	if p.bus != nil {
		p.bus.Publish(event)
	}
	if p.ui != nil && event.Type != events.TimingRecorded {
		p.ui.Show(event)
	}
}

func NewOrchestrator(options OrchestratorOptions) (*Orchestrator, error) {
	if options.SessionID == "" || options.RuntimeDir == "" {
		return nil, errs.New(errs.ConfigInvalid, "session", "identifiant ou répertoire de session manquant")
	}
	if options.Recorder == nil || options.Transcriber == nil || options.Processor == nil || options.Sink == nil {
		return nil, errs.New(errs.Internal, "session", "dépendances de l'orchestrateur incomplètes")
	}
	if options.Context == nil {
		options.Context = context.Background()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.UI == nil {
		options.UI = ui.NewNone()
	}
	ctx, cancel := context.WithCancel(context.Background())
	o := &Orchestrator{
		runtimeDir: options.RuntimeDir, recorder: options.Recorder, recordOpts: options.RecordOptions,
		chunk: options.ChunkDuration, live: options.LiveChunking, transcriber: options.Transcriber, processor: options.Processor,
		pauseWindow: options.ChunkPauseWindow, maxSegments: options.MaxSegments,
		overlap: options.ChunkOverlap,
		sink:    sink.Once(options.Sink), ui: options.UI, bus: options.Bus, now: options.Now,
		ctx: ctx, cancelOps: cancel, stop: make(chan struct{}, 1), runDone: make(chan struct{}),
		startedAt: options.StartedAt, parentCtx: options.Context,
	}
	o.machine = options.Machine
	if o.machine == nil {
		o.machine = NewMachine(options.SessionID, newPublicationGate(eventPublisher{bus: options.Bus, ui: options.UI}))
		o.machine.now = options.Now
	} else if o.machine.sessionID != options.SessionID {
		cancel()
		return nil, errs.New(errs.Internal, "session", "machine de bootstrap incohérente")
	} else {
		o.machine.SetPublisher(eventPublisher{bus: options.Bus, ui: options.UI})
	}
	o.ctx = events.WithTimings(o.ctx, func(timing events.Timing, provider string, partial int) {
		o.machine.Publish(events.Event{
			Version: 1, Type: events.TimingRecorded, Timestamp: o.now().UTC(), SessionID: o.machine.sessionID,
			State: string(o.machine.State()), Provider: provider, Partial: partial, Timing: &timing,
		})
	})
	go func() {
		select {
		case <-options.Context.Done():
			_ = o.Begin()
			_ = o.cancel()
		case <-o.runDone:
		}
	}()
	return o, nil
}

// Begin enters starting before the IPC socket is exposed.
func (o *Orchestrator) Begin() error {
	o.beginOnce.Do(func() {
		if o.startedAt.IsZero() {
			o.startedAt = o.now().UTC()
		}
		if o.parentCtx != nil && o.parentCtx.Err() != nil && o.machine.Result() == "" && o.machine.State() == Idle {
			o.beginErr = o.machine.ApplyTerminal(CancelCommand, o.currentProvider(), errs.Cancelled)
			o.cancelOps()
			return
		}
		if o.machine.Result() == "" && o.machine.State() == Idle {
			o.beginErr = o.machine.Apply(StartCommand)
		}
	})
	return o.beginErr
}

// Run keeps execution and persistence failures separate so callers cannot
// commit a terminal result after any failed write.
func (o *Orchestrator) Run() (LastResult, error, error) {
	o.runOnce.Do(func() {
		defer close(o.runDone)
		defer o.ui.Close()
		if err := o.Begin(); err != nil {
			o.runErr = err
			return
		}
		if o.machine.Result() == "" {
			o.runErr = o.run()
		}
		o.runResult = o.finalResult(o.runErr)
		o.persistErr = WriteLastResult(o.runtimeDir, o.runResult)
	})
	<-o.runDone
	return o.runResult, o.runErr, o.persistErr
}

func (o *Orchestrator) run() error {
	o.runStarted = time.Now()
	if o.live {
		return o.runLive()
	}
	return o.runCaptured()
}

func (o *Orchestrator) runCaptured() error {
	readyTiming := events.Measure(o.ctx, "capture_ready")
	recording, err := o.recorder.Start(o.ctx, o.recordOpts)
	if err != nil {
		return o.fail(err)
	}
	select {
	case readyErr := <-recording.Ready():
		if readyErr != nil {
			_ = recording.Abort()
			return o.fail(readyErr)
		}
	case <-recording.Done():
		err := audioRecordingFailure(recording.Err())
		_ = recording.Abort()
		if o.machine.Result() != "" {
			return nil
		}
		return o.fail(err)
	case <-o.ctx.Done():
		_ = recording.Abort()
		return nil
	}
	if err := o.machine.Apply(CaptureReadyCommand); err != nil {
		_ = recording.Abort()
		if o.machine.Result() != "" {
			return nil
		}
		return o.fail(err)
	}
	readyTiming()
	if o.machine.State() == Recording {
		select {
		case <-o.stop:
		case <-recording.Done():
			if o.machine.Result() != "" {
				_ = recording.Abort()
				return nil
			}
			if o.machine.State() != Stopping {
				err := audioRecordingFailure(recording.Err())
				_ = recording.Abort()
				return o.fail(err)
			}
		case <-o.ctx.Done():
			_ = recording.Abort()
			return nil
		}
	}

	file, err := recording.Stop(o.ctx)
	if err != nil {
		if o.machine.Result() != "" {
			return nil
		}
		if errs.Classify(err) == errs.NoSpeech {
			_ = o.machine.ApplyTerminal(NoSpeechCommand, o.currentProvider(), errs.NoSpeech)
			return nil
		}
		return o.fail(err)
	}
	if err := o.machine.Apply(CaptureStoppedCommand); err != nil {
		resultErr := o.fail(err)
		o.finishCapture(file, "")
		return resultErr
	}

	transcript, provider, warnings, err := o.transcribe(file)
	resultErr := o.completeTranscript(transcript, provider, warnings, err)
	o.finishCapture(file, provider)
	return resultErr
}

func (o *Orchestrator) runLive() error {
	readyTiming := events.Measure(o.ctx, "capture_ready")
	recorder, ok := o.recorder.(audio.LiveRecorder)
	if !ok {
		return o.fail(errs.New(errs.Internal, "session", "recorder incompatible avec le découpage live"))
	}
	transcriber, ok := o.transcriber.(LiveTranscriber)
	if !ok {
		return o.fail(errs.New(errs.Internal, "session", "transcriber incompatible avec le découpage live"))
	}
	recording, err := recorder.StartLive(o.ctx, o.recordOpts, audio.LiveOptions{
		SegmentDuration: o.chunk,
		MinimumTail:     time.Second,
		PauseWindow:     o.pauseWindow,
		Overlap:         o.overlap,
		MaxSegments:     o.maxSegments,
	})
	if err != nil {
		return o.fail(err)
	}
	// Stop may own the capture when Abort is called, in which case Abort only
	// observes its completion. Finalize every exit here, after the terminal
	// result has closed the cancellation window and all producers have stopped.
	defer func() { o.finishLiveCapture(recording, o.currentProvider()) }()
	select {
	case readyErr := <-recording.Ready():
		if readyErr != nil {
			_ = recording.Abort()
			return o.fail(readyErr)
		}
	case <-recording.Done():
		err := audioRecordingFailure(recording.Err())
		_ = recording.Abort()
		if o.machine.Result() != "" {
			return nil
		}
		return o.fail(err)
	case <-o.ctx.Done():
		_ = recording.Abort()
		return nil
	}
	if err := o.machine.Apply(CaptureReadyCommand); err != nil {
		_ = recording.Abort()
		if o.machine.Result() != "" {
			return nil
		}
		return o.fail(err)
	}
	readyTiming()

	requests := make(chan stt.Segment)
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		defer close(requests)
		for segment := range recording.Segments() {
			request := stt.Segment{Index: segment.Index, QueuedAt: segment.CreatedAt, Overlap: segment.Overlap, Request: stt.Request{
				Audio: segment.File, Filename: filepath.Base(segment.File.Path()),
			}}
			select {
			case requests <- request:
			case <-o.ctx.Done():
				return
			}
		}
	}()
	type transcriptionOutcome struct {
		result stt.MultiResult
		err    error
	}
	transcribed := make(chan transcriptionOutcome, 1)
	go func() {
		result, err := transcriber.TranscribeStream(o.ctx, requests, o.transcriptionProgress(0))
		transcribed <- transcriptionOutcome{result: result, err: err}
	}()

	if o.machine.State() == Recording {
		select {
		case <-o.stop:
		case outcome := <-transcribed:
			o.cancelOps()
			if stt.IsLimitExceeded(outcome.err) {
				abortErr := recording.Abort()
				<-forwarded
				return o.fail(errors.Join(outcome.err, abortErr))
			}
			err := audioRecordingFailure(recording.Err())
			_ = recording.Abort()
			<-forwarded
			return o.fail(errors.Join(err, outcome.err))
		case <-recording.Done():
			if o.machine.Result() != "" {
				_ = recording.Abort()
				<-forwarded
				<-transcribed
				return nil
			}
			if o.machine.State() != Stopping {
				err := audioRecordingFailure(recording.Err())
				o.cancelOps()
				_ = recording.Abort()
				<-forwarded
				<-transcribed
				return o.fail(err)
			}
		case <-o.ctx.Done():
			_ = recording.Abort()
			<-forwarded
			<-transcribed
			return nil
		}
	}
	// Stop may block while final segments are sent. Continue watching the STT
	// consumer so a fatal result can cancel the producer and release backpressure.
	stopped := make(chan error, 1)
	go func() { stopped <- recording.Stop(o.ctx) }()
	var stopErr error
	var earlyResult *transcriptionOutcome
	select {
	case stopErr = <-stopped:
	case outcome := <-transcribed:
		if outcome.err != nil {
			o.cancelOps()
			abortErr := recording.Abort()
			<-stopped
			<-forwarded
			// An expected cancellation from Abort must not mask the STT failure.
			if abortErr != nil && errs.Classify(abortErr) != errs.Cancelled {
				outcome.err = errors.Join(outcome.err, abortErr)
			}
			return o.fail(outcome.err)
		}
		// The producer may close normally just before Stop returns.
		earlyResult = &outcome
		stopErr = <-stopped
	}
	if stopErr != nil {
		<-forwarded
		if earlyResult == nil {
			<-transcribed
		}
		if o.machine.Result() != "" {
			return nil
		}
		if errs.Classify(stopErr) == errs.NoSpeech {
			_ = o.machine.ApplyTerminal(NoSpeechCommand, o.currentProvider(), errs.NoSpeech)
			return nil
		}
		return o.fail(stopErr)
	}
	if err := o.machine.Apply(CaptureStoppedCommand); err != nil {
		<-forwarded
		if earlyResult == nil {
			<-transcribed
		}
		return o.fail(err)
	}
	<-forwarded
	var outcome transcriptionOutcome
	if earlyResult != nil {
		outcome = *earlyResult
	} else {
		outcome = <-transcribed
	}
	provider := outcome.result.Provider
	if outcome.err == nil {
		o.publish(events.ProviderSelected, provider, "", "", 0, 0)
	}
	return o.completeTranscript(outcome.result.Text, provider, outcome.result.Warnings, outcome.err)
}

// A terminal result closes the cancellation window before retention is decided.
// Release must not run earlier: cancellation during processing would otherwise
// leave an already-preserved diagnostic WAV behind.
func (o *Orchestrator) finishCapture(file audio.WavFile, provider string) {
	if o.machine.Result() == Cancelled {
		o.recordAudioCleanupWarning(provider, file.Discard())
		return
	}
	_, cleanupErr := file.Release()
	o.recordAudioCleanupWarning(provider, cleanupErr)
}

func (o *Orchestrator) finishLiveCapture(recording audio.LiveRecording, provider string) {
	if o.machine.Result() == Cancelled {
		o.recordAudioCleanupWarning(provider, recording.Discard())
		return
	}
	_, cleanupErr := recording.Release()
	o.recordAudioCleanupWarning(provider, cleanupErr)
}

func (o *Orchestrator) recordAudioCleanupWarning(provider string, err error) {
	if err != nil {
		o.addWarning(ResultWarning{Class: errs.AudioFailed, Provider: provider, Component: "audio", Message: "nettoyage audio dégradé"})
	}
}

func (o *Orchestrator) completeTranscript(transcript, provider string, warnings []stt.Warning, err error) error {
	events.ReportTiming(o.ctx, events.Timing{Stage: "stt_complete", DurationMS: time.Since(o.runStarted).Milliseconds()}, "", 0)
	if err != nil {
		provider = stt.ProviderFromError(err)
		o.setProvider(provider)
		if o.machine.Result() != "" {
			return nil
		}
		if errs.Classify(err) == errs.EmptyResponse || errs.Classify(err) == errs.NoSpeech {
			_ = o.machine.ApplyTerminal(NoSpeechCommand, provider, errs.NoSpeech)
			return nil
		}
		return o.fail(err)
	}
	o.setProvider(provider)
	for _, warning := range warnings {
		o.addWarning(ResultWarning{
			Class: warning.Class, Provider: warning.Provider, Component: warning.Component, Message: warning.Message,
		})
		o.publish(events.Degraded, warning.Provider, warning.Class, warning.Component, 0, 0)
	}
	if err := o.machine.Apply(TranscriptionDoneCommand); err != nil {
		if o.machine.Result() != "" {
			return nil
		}
		return o.fail(err)
	}
	processed, err := o.processor.Run(o.ctx, transcript)
	if err != nil {
		if o.machine.Result() != "" {
			return nil
		}
		return o.fail(err)
	}
	for _, warning := range processed.Warnings {
		warningProvider := warning.Provider
		if warningProvider == "" {
			warningProvider = provider
		}
		o.addWarning(ResultWarning{
			Class: warning.Class, Provider: warningProvider, Component: warning.Component, Message: warning.Message,
		})
		o.publish(events.Degraded, warningProvider, warning.Class, warning.Component, 0, 0)
	}
	if strings.TrimSpace(processed.Text) == "" {
		_ = o.machine.ApplyTerminal(NoSpeechCommand, provider, errs.NoSpeech)
		return nil
	}
	if err := o.machine.Apply(ProcessingDoneCommand); err != nil {
		if o.machine.Result() != "" {
			return nil
		}
		return o.fail(err)
	}
	deliveryTiming := events.Measure(o.ctx, "delivery")
	if raw, ok := o.sink.(interface {
		CopyRaw(context.Context, string) error
	}); ok {
		if err := raw.CopyRaw(o.ctx, transcript); err != nil {
			deliveryTiming()
			return o.fail(err)
		}
	}
	if err := o.sink.Deliver(o.ctx, processed.Text); err != nil {
		deliveryTiming()
		return o.fail(err)
	}
	deliveryTiming()
	o.publish(events.Delivered, provider, "", "", 0, 0)
	o.metadataMu.Lock()
	stopAt := o.stopAt
	o.metadataMu.Unlock()
	if !stopAt.IsZero() {
		events.ReportTiming(o.ctx, events.Timing{Stage: "after_stop", DurationMS: time.Since(stopAt).Milliseconds()}, "", 0)
	}
	if err := o.machine.ApplyTerminal(DeliveryDoneCommand, provider, ""); err != nil {
		return o.fail(err)
	}
	return nil
}

func (o *Orchestrator) transcribe(file audio.WavFile) (string, string, []stt.Warning, error) {
	if o.chunk > 0 {
		count, err := file.File.SegmentCount(o.chunk, time.Second)
		if err != nil {
			return "", "", nil, err
		}
		if limiter, ok := o.transcriber.(interface{ ValidateSegmentCount(int) error }); ok {
			if err := limiter.ValidateSegmentCount(count); err != nil {
				return "", "", nil, err
			}
		}
	}
	options := audio.ChunkOptions{Duration: o.chunk, MinTail: time.Second, OutputDir: filepath.Dir(file.Path()), PauseWindow: o.pauseWindow, Overlap: o.overlap}
	if limiter, ok := o.transcriber.(interface{ ValidateSegmentCount(int) error }); ok {
		options.ValidateCount = limiter.ValidateSegmentCount
	}
	segments, err := audio.Chunk(file.File, options)
	if err != nil {
		return "", "", nil, err
	}
	requests := make([]stt.Segment, len(segments))
	for i, segment := range segments {
		requests[i] = stt.Segment{Index: segment.Index, Overlap: segment.Overlap, Request: stt.Request{
			Audio: segment.File, Filename: filepath.Base(segment.File.Path()),
		}}
	}
	result, err := o.transcriber.TranscribeSegments(o.ctx, requests, o.transcriptionProgress(len(segments)))
	segmentErr := audio.RemoveSegments(segments)
	if err != nil {
		return "", "", nil, errors.Join(err, segmentErr)
	}
	provider := result.Provider
	o.publish(events.ProviderSelected, provider, "", "", 0, 0)
	warnings := append([]stt.Warning(nil), result.Warnings...)
	if segmentErr != nil {
		warnings = append(warnings, stt.Warning{Class: errs.AudioFailed, Provider: provider, Component: "audio", Message: "nettoyage audio dégradé"})
	}
	return result.Text, provider, warnings, nil
}

func (o *Orchestrator) transcriptionProgress(total int) stt.Progress {
	return stt.Progress{
		Started: func(index int, provider string) {
			o.publish(events.PartialStarted, provider, "", "stt", index+1, total)
		},
		Done: func(index int, provider string) {
			o.publish(events.PartialDone, provider, "", "stt", index+1, total)
		},
		Failed: func(index int, provider string, class errs.Class) {
			o.publish(events.PartialFailed, provider, class, "stt", index+1, total)
		},
	}
}

func (o *Orchestrator) Handle(_ context.Context, request ipc.Request) ipc.Response {
	switch request.Command {
	case ipc.Status:
		return ipc.Success(string(o.machine.State()))
	case ipc.Stop:
		if err := o.machine.Apply(StopCommand); err != nil {
			return ipc.Failure(err)
		}
		o.requestStop()
		return ipc.Success(string(o.machine.State()))
	case ipc.Cancel:
		if err := o.cancel(); err != nil {
			return ipc.Failure(err)
		}
		return ipc.Success(string(o.machine.State()))
	case ipc.Toggle:
		if err := o.machine.Apply(ToggleCommand); err != nil {
			return ipc.Failure(err)
		}
		o.requestStop()
		return ipc.Success(string(o.machine.State()))
	default:
		return ipc.Failure(errs.New(errs.ConfigInvalid, "session", "commande IPC non gérée"))
	}
}

func (o *Orchestrator) requestStop() {
	o.stopOnce.Do(func() {
		o.metadataMu.Lock()
		o.stopAt = time.Now()
		o.metadataMu.Unlock()
		o.stop <- struct{}{}
	})
}

func (o *Orchestrator) cancel() error {
	switch o.machine.Result() {
	case Cancelled:
		return nil
	case Completed, NoSpeech, Failed:
		return errs.New(errs.Unavailable, "session", "annulation refusée: la session est déjà terminée")
	}
	if err := o.machine.ApplyTerminal(CancelCommand, o.currentProvider(), errs.Cancelled); err != nil {
		return err
	}
	o.cancelOps()
	return nil
}

func (o *Orchestrator) fail(err error) error {
	if o.machine.Result() != "" {
		return nil
	}
	if provider := stt.ProviderFromError(err); provider != "" {
		o.setProvider(provider)
	}
	if applyErr := o.machine.ApplyTerminal(FailCommand, o.currentProvider(), errs.Classify(err)); applyErr != nil {
		return errors.Join(err, applyErr)
	}
	return err
}

func audioRecordingFailure(err error) error {
	if errors.Is(err, audio.ErrSegmentLimit) {
		return err
	}
	if errs.Classify(err) == errs.AudioFailed {
		return err
	}
	if err == nil {
		return errs.New(errs.AudioFailed, "audio", "capture audio interrompue")
	}
	return errs.Wrap(errs.AudioFailed, "audio", "capture audio interrompue", err)
}

func (o *Orchestrator) finalResult(runErr error) LastResult {
	provider, warnings := o.metadata()
	result := o.machine.Result()
	if result == "" {
		result = Failed
		_ = o.machine.ApplyTerminal(FailCommand, provider, errs.Classify(runErr))
	}
	last := LastResult{
		Version: 1, SessionID: o.machine.sessionID, Result: result,
		StartedAt: o.startedAt, FinishedAt: o.now().UTC(),
		Provider: provider, Warnings: warnings,
	}
	if result == Failed && runErr != nil {
		last.Error = resultError(runErr)
	}
	return last
}

func (o *Orchestrator) setProvider(provider string) {
	o.metadataMu.Lock()
	o.runResult.Provider = provider
	o.metadataMu.Unlock()
}

func (o *Orchestrator) currentProvider() string {
	o.metadataMu.Lock()
	defer o.metadataMu.Unlock()
	return o.runResult.Provider
}

func (o *Orchestrator) addWarning(warning ResultWarning) {
	o.metadataMu.Lock()
	o.runResult.Warnings = append(o.runResult.Warnings, warning)
	o.metadataMu.Unlock()
}

func (o *Orchestrator) metadata() (string, []ResultWarning) {
	o.metadataMu.Lock()
	defer o.metadataMu.Unlock()
	return o.runResult.Provider, append([]ResultWarning(nil), o.runResult.Warnings...)
}

func resultError(err error) *ResultError {
	result := &ResultError{Class: errs.Classify(err), Message: "échec de la session"}
	var typed *errs.Error
	if errors.As(err, &typed) {
		result.Component = typed.Component
		result.Message = typed.Msg
	}
	return result
}

func (o *Orchestrator) publish(kind events.Type, provider string, class errs.Class, component string, partial, total int) {
	o.machine.Publish(events.Event{
		Version: 1, Type: kind, Timestamp: o.now().UTC(), SessionID: o.machine.sessionID,
		State: string(o.machine.State()), Provider: provider, ErrorClass: class,
		Component: component, Partial: partial, Total: total,
	})
}

func (o *Orchestrator) String() string {
	return fmt.Sprintf("session %s (%s)", o.machine.sessionID, o.machine.State())
}

var _ ipc.Handler = (*Orchestrator)(nil)
