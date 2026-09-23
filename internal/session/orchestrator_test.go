package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/audio/pwrecord"
	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/ipc"
	"git.2027a.net/mathieu/murmur/internal/sink"
	"git.2027a.net/mathieu/murmur/internal/stt"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
	"git.2027a.net/mathieu/murmur/internal/textpipe"
	"git.2027a.net/mathieu/murmur/internal/ui"
)

type fakeTranscriber struct {
	started chan struct{}
	gate    <-chan struct{}
	result  stt.MultiResult
	err     error
	once    sync.Once
}

type fixedTextProvider struct {
	text string
}

func (p fixedTextProvider) Name() string { return "primary" }
func (p fixedTextProvider) Transcribe(context.Context, stt.Request) (stt.Result, error) {
	return stt.Result{Text: p.text}, nil
}

type intermediateFailureProvider struct {
	mu       sync.Mutex
	calls    int
	text     string
	failCall int
}

func (p *intermediateFailureProvider) Name() string { return "primary" }
func (p *intermediateFailureProvider) Transcribe(context.Context, stt.Request) (stt.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	failCall := p.failCall
	if failCall == 0 {
		failCall = 2
	}
	if p.calls == failCall {
		return stt.Result{}, errs.New(errs.ServerError, "fake", "down")
	}
	if p.text != "" {
		return stt.Result{Text: p.text}, nil
	}
	return stt.Result{Text: "ok"}, nil
}

func (f *fakeTranscriber) TranscribeSegments(ctx context.Context, _ []stt.Segment, _ stt.Progress) (stt.MultiResult, error) {
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return stt.MultiResult{}, ctx.Err()
		}
	}
	return f.result, f.err
}

func (f *fakeTranscriber) TranscribeStream(ctx context.Context, segments <-chan stt.Segment, _ stt.Progress) (stt.MultiResult, error) {
	for {
		select {
		case _, ok := <-segments:
			if !ok {
				return f.TranscribeSegments(ctx, nil, stt.Progress{})
			}
		case <-ctx.Done():
			return stt.MultiResult{}, ctx.Err()
		}
	}
}

type fakeProcessor struct {
	started chan struct{}
	gate    <-chan struct{}
	result  textpipe.Result
	err     error
	once    sync.Once
}

func (f *fakeProcessor) Run(ctx context.Context, _ string) (textpipe.Result, error) {
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return textpipe.Result{}, ctx.Err()
		}
	}
	return f.result, f.err
}

type fakeSink struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	gate    <-chan struct{}
	err     error
	once    sync.Once
}

type observedRecorder struct {
	audio.Recorder
	started chan<- audio.Recording
}

func (r observedRecorder) Start(ctx context.Context, opts audio.RecordOptions) (audio.Recording, error) {
	recording, err := r.Recorder.Start(ctx, opts)
	if err == nil {
		r.started <- recording
	}
	return recording, err
}

type stopObservedRecorder struct {
	*audio.FakeRecorder
	stopStarted chan struct{}
}

func (r stopObservedRecorder) StartLive(ctx context.Context, opts audio.RecordOptions, live audio.LiveOptions) (audio.LiveRecording, error) {
	recording, err := r.FakeRecorder.StartLive(ctx, opts, live)
	if err != nil {
		return nil, err
	}
	return stopObservedRecording{LiveRecording: recording, started: r.stopStarted}, nil
}

type stopObservedRecording struct {
	audio.LiveRecording
	started chan struct{}
}

func (r stopObservedRecording) Stop(ctx context.Context) error {
	close(r.started)
	return r.LiveRecording.Stop(ctx)
}

type gatedLiveProvider struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (p *gatedLiveProvider) Name() string { return "primary" }
func (p *gatedLiveProvider) Transcribe(ctx context.Context, _ stt.Request) (stt.Result, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return stt.Result{Text: "éé"}, nil
	case <-ctx.Done():
		return stt.Result{}, ctx.Err()
	}
}

type countingStartRecorder struct {
	calls atomic.Int32
}

func (r *countingStartRecorder) Start(context.Context, audio.RecordOptions) (audio.Recording, error) {
	r.calls.Add(1)
	return nil, errors.New("unexpected recorder start")
}

type blockingStateUI struct {
	state   State
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *blockingStateUI) Show(event events.Event) {
	if event.State != string(u.state) {
		return
	}
	u.once.Do(func() {
		close(u.entered)
		<-u.release
	})
}

func (*blockingStateUI) Close() {}

type timedFailureSink struct {
	active *atomic.Int32
}

func (s timedFailureSink) Deliver(context.Context, string) error {
	s.active.Add(1)
	defer s.active.Add(-1)
	time.Sleep(20 * time.Millisecond)
	return errs.New(errs.SinkFailed, "sink/test", "délai de livraison dépassé")
}

func (f *fakeSink) Deliver(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestOrchestrator(t *testing.T, ready <-chan struct{}, transcriber Transcriber, processor Processor, target sink.Sink) *Orchestrator {
	t.Helper()
	if transcriber == nil {
		transcriber = &fakeTranscriber{result: stt.MultiResult{
			Text: "brut", Provider: "primary",
			Segments: []stt.SegmentResult{{Index: 0, Text: "brut", Provider: "primary"}},
		}}
	}
	if processor == nil {
		processor = &fakeProcessor{result: textpipe.Result{Text: "final"}}
	}
	if target == nil {
		target = &fakeSink{}
	}
	runtimeDir := t.TempDir()
	orchestrator, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "test-session", RuntimeDir: runtimeDir,
		Recorder:      &audio.FakeRecorder{FixturePath: sessionWAV(t), ReadyGate: ready},
		RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		Transcriber:   transcriber, Processor: processor, Sink: target,
		UI: ui.NewNone(), Bus: events.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator
}

func TestStopDuringStartingCompletes(t *testing.T) {
	ready := make(chan struct{})
	target := &fakeSink{}
	o := newTestOrchestrator(t, ready, nil, nil, target)
	if err := o.Begin(); err != nil {
		t.Fatal(err)
	}
	done := runOrchestrator(o)
	response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
	if !response.OK || response.State != string(Starting) || !o.machine.StopPending() {
		t.Fatalf("stop response=%#v pending=%v", response, o.machine.StopPending())
	}
	close(ready)
	result := awaitResult(t, done)
	if result.Result != Completed || target.count() != 1 {
		t.Fatalf("result=%#v sink calls=%d", result, target.count())
	}
}

func TestParentCancelledBeforeRunCallsNoComponentAndPersistsSingleCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtimeDir := t.TempDir()
	recorder := &countingStartRecorder{}
	transcriber := &fakeTranscriber{started: make(chan struct{})}
	processor := &fakeProcessor{started: make(chan struct{})}
	target := &fakeSink{started: make(chan struct{})}
	bus := events.NewBus()
	defer bus.Close()
	stream, unsubscribe := bus.Subscribe(16)
	defer unsubscribe()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: ctx, SessionID: "pre-cancelled", RuntimeDir: runtimeDir,
		Recorder: recorder, RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		Transcriber: transcriber, Processor: processor, Sink: target,
		UI: ui.NewNone(), Bus: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr, persistErr := o.Run()
	if runErr != nil || persistErr != nil || result.Result != Cancelled {
		t.Fatalf("result=%#v runErr=%v persistErr=%v", result, runErr, persistErr)
	}
	if recorder.calls.Load() != 0 || target.count() != 0 {
		t.Fatalf("components called: recorder=%d sink=%d", recorder.calls.Load(), target.count())
	}
	select {
	case <-transcriber.started:
		t.Fatal("transcriber called")
	default:
	}
	select {
	case <-processor.started:
		t.Fatal("processor called")
	default:
	}
	terminal := 0
	for len(stream) > 0 {
		if event := <-stream; event.Type == events.Terminal {
			terminal++
			if event.Result != string(Cancelled) {
				t.Fatalf("terminal event=%#v", event)
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal events=%d, want 1", terminal)
	}
	persisted, err := ReadLastResult(runtimeDir)
	if err != nil || persisted.Result != Cancelled {
		t.Fatalf("persisted=%#v error=%v", persisted, err)
	}
}

func TestAudioDeathAfterReadinessFailsAndPersists(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "normal"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			died := make(chan struct{})
			target := &fakeSink{}
			transcriber := &fakeTranscriber{gate: make(chan struct{}), result: stt.MultiResult{
				Text: "brut", Provider: "primary",
				Segments: []stt.SegmentResult{{Index: 0, Text: "brut", Provider: "primary"}},
			}}
			o, err := NewOrchestrator(OrchestratorOptions{
				Context: context.Background(), SessionID: "audio-death-" + name, RuntimeDir: runtimeDir,
				Recorder: &audio.FakeRecorder{
					FixturePath: sessionWAV(t), DoneGate: died, DoneErr: errors.New("pw-record died"),
				},
				RecordOptions: audio.RecordOptions{Directory: runtimeDir},
				ChunkDuration: 100 * time.Millisecond, LiveChunking: live,
				Transcriber: transcriber, Processor: &fakeProcessor{result: textpipe.Result{Text: "final"}},
				Sink: target, UI: ui.NewNone(), Bus: events.NewBus(),
			})
			if err != nil {
				t.Fatal(err)
			}
			done := runOrchestrator(o)
			waitForState(t, o, Recording)
			started := time.Now()
			close(died)
			result := awaitResult(t, done)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("terminaison audio propagée en %s", elapsed)
			}
			if result.Result != Failed || result.Error == nil || result.Error.Class != errs.AudioFailed || target.count() != 0 {
				t.Fatalf("result=%#v sink calls=%d", result, target.count())
			}
			persisted, err := ReadLastResult(runtimeDir)
			if err != nil || persisted.Result != Failed || persisted.Error == nil || persisted.Error.Class != errs.AudioFailed {
				t.Fatalf("persisted=%#v error=%v", persisted, err)
			}
		})
	}
}

func TestPendingStopAfterObservedAudioDeathNeverReachesSTT(t *testing.T) {
	runtimeDir := t.TempDir()
	fixture, err := os.ReadFile(sessionWAV(t))
	if err != nil {
		t.Fatal(err)
	}
	process := sysexec.NewFakeProcess()
	var recordingPath string
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		recordingPath = args[len(args)-1]
		if err := wav.CreateGrowing(recordingPath); err != nil {
			return nil, err
		}
		return process, nil
	}}
	started := make(chan audio.Recording, 1)
	recorder := observedRecorder{Recorder: pwrecord.New(executor), started: started}
	blocked := &blockingStateUI{state: Stopping, entered: make(chan struct{}), release: make(chan struct{})}
	transcriptionStarted := make(chan struct{})
	transcriber := &fakeTranscriber{
		started: transcriptionStarted,
		result: stt.MultiResult{
			Text: "ne doit pas être produit", Provider: "primary",
			Segments: []stt.SegmentResult{{Index: 0, Text: "ne doit pas être produit", Provider: "primary"}},
		},
	}
	target := &fakeSink{}
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "stop-audio-death-race", RuntimeDir: runtimeDir,
		Recorder: recorder,
		RecordOptions: audio.RecordOptions{
			Directory: runtimeDir, ReadinessInterval: time.Millisecond, MinimumDuration: 100 * time.Millisecond,
		},
		Transcriber: transcriber, Processor: &fakeProcessor{result: textpipe.Result{Text: "final"}},
		Sink: target, UI: blocked, Bus: events.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Begin(); err != nil {
		t.Fatal(err)
	}
	done := runOrchestrator(o)
	var recording audio.Recording
	select {
	case recording = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("recorder non démarré")
	}
	if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK || !o.machine.StopPending() {
		t.Fatalf("stop response=%#v pending=%v", response, o.machine.StopPending())
	}
	readyPath := recordingPath + ".ready"
	if err := os.WriteFile(readyPath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(readyPath, recordingPath); err != nil {
		t.Fatal(err)
	}
	await(t, blocked.entered)
	process.Exit(errors.New("pw-record died"))
	await(t, recording.Done())
	if err := recording.Err(); errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("erreur observée avant Stop=%v", err)
	}
	if signals := process.Signals(); len(signals) != 0 {
		t.Fatalf("signaux avant Stop=%v", signals)
	}
	close(blocked.release)
	result := awaitResult(t, done)
	if result.Result != Failed || result.Error == nil || result.Error.Class != errs.AudioFailed {
		t.Fatalf("result=%#v", result)
	}
	select {
	case <-transcriptionStarted:
		t.Fatal("le WAV d'une mort spontanée a atteint le STT")
	default:
	}
	if target.count() != 0 {
		t.Fatalf("sink calls=%d", target.count())
	}
	if signals := process.Signals(); len(signals) != 0 {
		t.Fatalf("signaux après Stop=%v", signals)
	}
}

func TestRequestedStopDoesNotBecomeAudioFailure(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "normal"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			target := &fakeSink{}
			transcriber := &fakeTranscriber{result: stt.MultiResult{
				Text: "brut", Provider: "primary",
				Segments: []stt.SegmentResult{{Index: 0, Text: "brut", Provider: "primary"}},
			}}
			o, err := NewOrchestrator(OrchestratorOptions{
				Context: context.Background(), SessionID: "normal-stop-" + name, RuntimeDir: runtimeDir,
				Recorder:      &audio.FakeRecorder{FixturePath: sessionWAV(t)},
				RecordOptions: audio.RecordOptions{Directory: runtimeDir},
				ChunkDuration: 100 * time.Millisecond, LiveChunking: live,
				Transcriber: transcriber, Processor: &fakeProcessor{result: textpipe.Result{Text: "final"}},
				Sink: target, UI: ui.NewNone(), Bus: events.NewBus(),
			})
			if err != nil {
				t.Fatal(err)
			}
			done := runOrchestrator(o)
			waitForState(t, o, Recording)
			if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
				t.Fatalf("stop response=%#v", response)
			}
			result := awaitResult(t, done)
			if result.Result != Completed || result.Error != nil || target.count() != 1 {
				t.Fatalf("result=%#v sink calls=%d", result, target.count())
			}
		})
	}
}

func TestAcceptance_09_CancelBeforeDeliveringNeverCallsSink(t *testing.T) {
	started := make(chan struct{})
	transcriber := &fakeTranscriber{started: started, gate: make(chan struct{})}
	target := &fakeSink{}
	o := newTestOrchestrator(t, nil, transcriber, nil, target)
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
	await(t, started)
	response := o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel))
	if !response.OK {
		t.Fatalf("cancel response=%#v", response)
	}
	result := awaitResult(t, done)
	if result.Result != Cancelled || target.count() != 0 {
		t.Fatalf("result=%#v sink calls=%d", result, target.count())
	}
}

func TestCancelledSessionNeverRetainsAudio(t *testing.T) {
	for _, live := range []bool{false, true} {
		for _, stage := range []string{"transcribing", "processing", "completed"} {
			for _, keep := range []bool{false, true} {
				name := "batch"
				if live {
					name = "live"
				}
				t.Run(name+"/"+stage+"/keep="+strconv.FormatBool(keep), func(t *testing.T) {
					root := t.TempDir()
					diagnostics := filepath.Join(root, "diagnostics")
					started := make(chan struct{})
					transcriber := &fakeTranscriber{result: stt.MultiResult{Text: "brut", Provider: "primary"}}
					processor := &fakeProcessor{result: textpipe.Result{Text: "final"}}
					if stage == "transcribing" {
						transcriber.started = started
						transcriber.gate = make(chan struct{})
					} else if stage == "processing" {
						processor.started = started
						processor.gate = make(chan struct{})
					}
					target := &fakeSink{}
					o, err := NewOrchestrator(OrchestratorOptions{
						Context: context.Background(), SessionID: "retention-" + name, RuntimeDir: root,
						Recorder:      &audio.FakeRecorder{FixturePath: sessionWAVDuration(t, 2*time.Second)},
						RecordOptions: audio.RecordOptions{Directory: root, Retention: audio.RetentionOptions{Keep: keep, Directory: diagnostics}},
						ChunkDuration: 100 * time.Millisecond, LiveChunking: live,
						Transcriber: transcriber, Processor: processor, Sink: target, UI: ui.NewNone(), Bus: events.NewBus(),
					})
					if err != nil {
						t.Fatal(err)
					}
					done := runOrchestrator(o)
					waitForState(t, o, Recording)
					if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
						t.Fatalf("stop response=%#v", response)
					}
					if stage != "completed" {
						await(t, started)
						if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel)); !response.OK {
							t.Fatalf("cancel response=%#v", response)
						}
					}
					result := awaitResult(t, done)
					want := Cancelled
					if stage == "completed" {
						want = Completed
					}
					if result.Result != want || (stage != "completed" && target.count() != 0) || (stage == "completed" && target.count() != 1) {
						t.Fatalf("result=%#v error=%+v sink calls=%d", result, result.Error, target.count())
					}
					files, err := filepath.Glob(filepath.Join(diagnostics, "recording-*.wav"))
					if err != nil {
						t.Fatal(err)
					}
					wantFiles := 0
					if stage == "completed" && keep {
						wantFiles = 1
					}
					if len(files) != wantFiles {
						t.Fatalf("diagnostic WAVs=%v want=%d", files, wantFiles)
					}
					captures, err := filepath.Glob(filepath.Join(root, "murmur-audio-*"))
					if err != nil || len(captures) != 0 {
						t.Fatalf("capture leftovers=%v error=%v", captures, err)
					}
				})
			}
		}
	}
}

func TestLateCancelIsRejectedAndDeliveryOccursExactlyOnce(t *testing.T) {
	started := make(chan struct{})
	gate := make(chan struct{})
	target := &fakeSink{started: started, gate: gate}
	o := newTestOrchestrator(t, nil, nil, nil, target)
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
		t.Fatalf("stop response=%#v", response)
	}
	await(t, started)
	response := o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel))
	if response.OK || response.Error == nil || response.Error.Class != errs.Unavailable {
		t.Fatalf("late cancel response=%#v", response)
	}
	close(gate)
	result := awaitResult(t, done)
	if result.Result != Completed || target.count() != 1 {
		t.Fatalf("result=%#v sink calls=%d", result, target.count())
	}
}

func TestConcurrentStopAndCancelPersistsCancelled(t *testing.T) {
	for range 20 {
		transcriber := &fakeTranscriber{gate: make(chan struct{})}
		o := newTestOrchestrator(t, nil, transcriber, nil, &fakeSink{})
		done := runOrchestrator(o)
		waitForState(t, o, Recording)
		start := make(chan struct{})
		var commands sync.WaitGroup
		commands.Add(2)
		go func() { defer commands.Done(); <-start; _ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)) }()
		go func() { defer commands.Done(); <-start; _ = o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel)) }()
		close(start)
		commands.Wait()
		result := awaitResult(t, done)
		if result.Result != Cancelled {
			t.Fatalf("result=%#v", result)
		}
		persisted, err := ReadLastResult(o.runtimeDir)
		if err != nil || persisted.Result != Cancelled {
			t.Fatalf("persisted=%#v error=%v", persisted, err)
		}
	}
}

func TestSinkFailurePersistsEffectiveProviderAndClass(t *testing.T) {
	target := &fakeSink{err: errs.New(errs.SinkFailed, "fake-sink", "refus")}
	o := newTestOrchestrator(t, nil, nil, nil, target)
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
	result := awaitResult(t, done)
	if result.Result != Failed || result.Provider != "primary" || result.Error == nil || result.Error.Class != errs.SinkFailed {
		t.Fatalf("result=%#v", result)
	}
	persisted, err := ReadLastResult(o.runtimeDir)
	if err != nil || persisted.Error == nil || persisted.Error.Class != errs.SinkFailed {
		t.Fatalf("persisted=%#v error=%v", persisted, err)
	}
}

func TestPipelineWarningClassesReachEventsAndLastResult(t *testing.T) {
	const canary = "MURMUR-SENSITIVE-SESSION-CANARY"
	for _, class := range []errs.Class{errs.ServerError, errs.Timeout, errs.AuthFailed, errs.PolicyBlocked, errs.Unavailable} {
		t.Run(string(class), func(t *testing.T) {
			pipeline, err := textpipe.New(textpipe.Options{
				CommandPack:     "fr",
				CleanupProvider: "cleanup",
				Cleanup: textpipe.CleanupFunc(func(context.Context, string, textpipe.Protection) (string, error) {
					return "", errs.Wrap(class, "unsafe/"+canary, "unsafe "+canary, errors.New("cause "+canary))
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			o := newTestOrchestrator(t, nil, nil, pipeline, &fakeSink{})
			stream, unsubscribe := o.bus.Subscribe(32)
			defer unsubscribe()
			done := runOrchestrator(o)
			waitForState(t, o, Recording)
			_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
			result := awaitResult(t, done)
			if result.Result != Completed || len(result.Warnings) != 1 {
				t.Fatalf("result=%#v", result)
			}
			warning := result.Warnings[0]
			if warning.Class != class || warning.Provider != "cleanup" || warning.Component != "cleanup" {
				t.Fatalf("warning=%#v", warning)
			}
			persisted, err := ReadLastResult(o.runtimeDir)
			if err != nil || len(persisted.Warnings) != 1 || persisted.Warnings[0].Class != class ||
				persisted.Warnings[0].Provider != "cleanup" {
				t.Fatalf("persisted=%#v error=%v", persisted, err)
			}
			encoded, err := json.Marshal(persisted)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), canary) {
				t.Fatalf("sensitive pipeline error leaked in %s", encoded)
			}
			var degraded bool
			for len(stream) > 0 {
				event := <-stream
				degraded = degraded || event.Type == events.Degraded && event.Provider == "cleanup" &&
					event.ErrorClass == class && event.Component == "cleanup"
			}
			if !degraded {
				t.Fatalf("classified degraded event missing from %s", class)
			}
		})
	}
}

func TestOrchestratorPreservesGlobalProviderWithoutSegmentInference(t *testing.T) {
	for _, provider := range []string{stt.ProviderMixed, ""} {
		name := provider
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			transcriber := &fakeTranscriber{result: stt.MultiResult{
				Text: "one two", Provider: provider,
				Segments: []stt.SegmentResult{
					{Index: 0, Text: "one", Provider: "primary"},
					{Index: 1, Text: "two", Provider: "fallback"},
				},
			}}
			o := newTestOrchestrator(t, nil, transcriber, nil, &fakeSink{})
			stream, unsubscribe := o.bus.Subscribe(32)
			defer unsubscribe()
			done := runOrchestrator(o)
			waitForState(t, o, Recording)
			_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
			result := awaitResult(t, done)
			if result.Result != Completed || result.Provider != provider {
				t.Fatalf("result=%#v", result)
			}
			persisted, err := ReadLastResult(o.runtimeDir)
			if err != nil || persisted.Provider != provider {
				t.Fatalf("persisted=%#v error=%v", persisted, err)
			}
			var selected, terminal bool
			for len(stream) > 0 {
				event := <-stream
				if event.Type == events.ProviderSelected && event.Provider == provider {
					selected = true
				}
				if event.Type == events.Terminal && event.Provider == provider {
					terminal = true
				}
			}
			if !selected || !terminal {
				t.Fatalf("global provider events: selected=%v terminal=%v", selected, terminal)
			}
		})
	}
}

func TestTranscriptBudgetFailureNeverCallsProcessorOrSink(t *testing.T) {
	chain, err := stt.NewChain(fixedTextProvider{text: "éé"}, nil, stt.MultiOptions{MaxTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	processorStarted := make(chan struct{})
	target := &fakeSink{}
	runtimeDir := t.TempDir()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "text-budget", RuntimeDir: runtimeDir,
		Recorder:      &audio.FakeRecorder{FixturePath: sessionWAVDuration(t, 3*time.Second)},
		RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		ChunkDuration: time.Second,
		Transcriber:   chain,
		Processor:     &fakeProcessor{started: processorStarted, result: textpipe.Result{Text: "final"}},
		Sink:          target,
		UI:            ui.NewNone(), Bus: events.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
		t.Fatalf("stop response=%#v", response)
	}
	result := awaitResult(t, done)
	if result.Result != Failed || result.Provider != "primary" || result.Error == nil || result.Error.Class != errs.ServerError || result.Error.Message != stt.TextBudgetExceeded {
		t.Fatalf("result=%#v", result)
	}
	select {
	case <-processorStarted:
		t.Fatal("processor called after transcript budget overflow")
	default:
	}
	if target.count() != 0 {
		t.Fatalf("sink calls=%d", target.count())
	}
}

func TestBatchPartialFailureThenBudgetOverflowNeverDelivers(t *testing.T) {
	// The first error is recoverable; the third segment exceeds the cumulative
	// UTF-8 budget including its separator and must remain fatal.
	provider := &intermediateFailureProvider{text: "éé", failCall: 1}
	chain, err := stt.NewChain(provider, nil, stt.MultiOptions{AllowPartial: true, MaxTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	processorStarted := make(chan struct{})
	target := &fakeSink{}
	runtimeDir := t.TempDir()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "partial-budget", RuntimeDir: runtimeDir,
		Recorder:      &audio.FakeRecorder{FixturePath: sessionWAVDuration(t, 3*time.Second)},
		RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		ChunkDuration: time.Second,
		Transcriber:   chain,
		Processor:     &fakeProcessor{started: processorStarted, result: textpipe.Result{Text: "final"}},
		Sink:          target, UI: ui.NewNone(), Bus: events.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
		t.Fatalf("stop response=%#v", response)
	}
	result := awaitResult(t, done)
	if result.Result != Failed || result.Provider != "primary" || result.Error == nil || result.Error.Class != errs.ServerError || result.Error.Message != stt.TextBudgetExceeded {
		t.Fatalf("result=%#v", result)
	}
	select {
	case <-processorStarted:
		t.Fatal("processor called after transcript budget overflow")
	default:
	}
	if target.count() != 0 {
		t.Fatalf("sink calls=%d", target.count())
	}
}

func TestCapturedPartialEventsKeepPublicSegmentIndexes(t *testing.T) {
	provider := &intermediateFailureProvider{}
	chain, err := stt.NewChain(provider, nil, stt.MultiOptions{AllowPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	stream, unsubscribe := bus.Subscribe(64)
	defer unsubscribe()
	runtimeDir := t.TempDir()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "captured-progress", RuntimeDir: runtimeDir,
		Recorder:      &audio.FakeRecorder{FixturePath: sessionWAVDuration(t, 3*time.Second)},
		RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		ChunkDuration: time.Second,
		Transcriber:   chain,
		Processor:     &fakeProcessor{result: textpipe.Result{Text: "final"}},
		Sink:          &fakeSink{}, UI: ui.NewNone(), Bus: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
		t.Fatalf("stop response=%#v", response)
	}
	if result := awaitResult(t, done); result.Result != Completed {
		t.Fatalf("result=%#v", result)
	}

	var got []events.Event
	for len(stream) > 0 {
		event := <-stream
		if event.Type == events.PartialStarted || event.Type == events.PartialDone || event.Type == events.PartialFailed {
			got = append(got, event)
		}
	}
	wantTypes := []events.Type{
		events.PartialStarted, events.PartialDone,
		events.PartialStarted, events.PartialFailed,
		events.PartialStarted, events.PartialDone,
	}
	wantIndexes := []int{1, 1, 2, 2, 3, 3}
	if len(got) != len(wantTypes) {
		t.Fatalf("partial events=%#v", got)
	}
	for i, event := range got {
		if event.Type != wantTypes[i] || event.Partial != wantIndexes[i] || event.Total != 3 ||
			event.Provider != "primary" || event.Component != "stt" {
			t.Fatalf("partial event %d=%#v", i, event)
		}
		if event.Type == events.PartialFailed && event.ErrorClass != errs.ServerError {
			t.Fatalf("failed event=%#v", event)
		}
	}
}

func TestLiveTranscriptBudgetAbortsCaptureWithoutStopOrSink(t *testing.T) {
	chain, err := stt.NewChain(fixedTextProvider{text: "éé"}, nil, stt.MultiOptions{Workers: 2, MaxTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	processorStarted := make(chan struct{})
	target := &fakeSink{}
	runtimeDir := t.TempDir()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "live-text-budget", RuntimeDir: runtimeDir,
		Recorder:      &audio.FakeRecorder{FixturePath: sessionWAVDuration(t, 5*time.Second)},
		RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		ChunkDuration: time.Second,
		LiveChunking:  true,
		Transcriber:   chain,
		Processor:     &fakeProcessor{started: processorStarted, result: textpipe.Result{Text: "final"}},
		Sink:          target,
		UI:            ui.NewNone(), Bus: events.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := awaitResult(t, runOrchestrator(o))
	if result.Result != Failed || result.Provider != "primary" || result.Error == nil || result.Error.Class != errs.ServerError || result.Error.Message != stt.TextBudgetExceeded {
		t.Fatalf("result=%#v", result)
	}
	select {
	case <-processorStarted:
		t.Fatal("processor called after live transcript budget overflow")
	default:
	}
	if target.count() != 0 {
		t.Fatalf("sink calls=%d", target.count())
	}
}

func TestLiveStopDrainsAfterFatalSTT(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit       int
		maxSegments int
		cancel      bool
		want        Result
		message     string
	}{
		{name: "fatal budget", limit: 2, want: Failed, message: stt.TextBudgetExceeded},
		{name: "fatal segment count", limit: 1024, maxSegments: 1, want: Failed, message: stt.SegmentLimitExceeded},
		{name: "within budget", limit: 1024, want: Completed},
		{name: "cancel during stop", limit: 1024, cancel: true, want: Cancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			provider := &gatedLiveProvider{started: make(chan struct{}), release: release}
			chain, err := stt.NewChain(provider, nil, stt.MultiOptions{Workers: 1, MaxTextBytes: tc.limit, MaxSegments: tc.maxSegments})
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			stopStarted := make(chan struct{})
			target := &fakeSink{}
			o, err := NewOrchestrator(OrchestratorOptions{
				Context: context.Background(), SessionID: "live-stop-drain", RuntimeDir: root,
				Recorder:      stopObservedRecorder{FakeRecorder: &audio.FakeRecorder{FixturePath: sessionWAVDuration(t, 5*time.Second), LiveStepInterval: time.Millisecond}, stopStarted: stopStarted},
				RecordOptions: audio.RecordOptions{Directory: root}, ChunkDuration: 100 * time.Millisecond,
				LiveChunking: true, Transcriber: chain, Processor: &fakeProcessor{result: textpipe.Result{Text: "final"}},
				Sink: target, UI: ui.NewNone(), Bus: events.NewBus(),
			})
			if err != nil {
				t.Fatal(err)
			}
			done := runOrchestrator(o)
			select {
			case <-provider.started:
			case <-time.After(5 * time.Second):
				t.Fatal("no live segment reached STT")
			}
			if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
				t.Fatalf("stop response=%#v", response)
			}
			select {
			case <-stopStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("audio stop not started")
			}
			if tc.cancel {
				if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel)); !response.OK {
					t.Fatalf("cancel response=%#v", response)
				}
			}
			close(release)
			result := awaitResult(t, done)
			if result.Result != tc.want {
				t.Fatalf("result=%#v", result)
			}
			if tc.want == Failed {
				if result.Provider != "primary" || result.Error == nil || result.Error.Class != errs.ServerError || result.Error.Message != tc.message || target.count() != 0 {
					t.Fatalf("result=%#v sink calls=%d", result, target.count())
				}
			} else if tc.want == Completed && target.count() != 1 {
				t.Fatalf("sink calls=%d", target.count())
			} else if tc.want == Cancelled && target.count() != 0 {
				t.Fatalf("sink called after cancellation: %d", target.count())
			}
		})
	}
}

func TestDeliveryTimeoutReleasesSessionResourcesForNextSession(t *testing.T) {
	runtimeDir := shortTempDir(t)
	lock, err := AcquireLock(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	bus := events.NewBus()
	defer bus.Close()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "timed-out-delivery", RuntimeDir: runtimeDir,
		Recorder: &audio.FakeRecorder{FixturePath: sessionWAV(t)}, RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		Transcriber: &fakeTranscriber{result: stt.MultiResult{
			Text: "raw", Provider: "primary", Segments: []stt.SegmentResult{{Provider: "primary"}},
		}},
		Processor: &fakeProcessor{result: textpipe.Result{Text: "final"}}, Sink: timedFailureSink{active: &active}, UI: ui.NewNone(), Bus: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(runtimeDir, "session.sock")
	server, err := ipc.Listen(socket, o, bus)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServer := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx) }()

	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
		t.Fatalf("stop response = %#v", response)
	}
	result := awaitResult(t, done)
	if result.Result != Failed || result.Error == nil || result.Error.Class != errs.SinkFailed {
		t.Fatalf("result = %#v", result)
	}
	if active.Load() != 0 {
		t.Fatalf("commandes encore actives = %d", active.Load())
	}

	stopServer()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	nextLock, err := AcquireLock(runtimeDir)
	if err != nil {
		t.Fatalf("session suivante bloquée par le verrou: %v", err)
	}
	defer nextLock.Close()
	nextServer, err := ipc.Listen(socket, o, bus)
	if err != nil {
		t.Fatalf("session suivante bloquée par le socket: %v", err)
	}
	if err := nextServer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestFakeIntegrationStartStatusCancelCycle(t *testing.T) {
	o := newTestOrchestrator(t, nil, nil, nil, &fakeSink{})
	if err := o.Begin(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shortTempDir(t), "session.sock")
	server, err := ipc.Listen(path, o, o.bus)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServer := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx) }()
	done := runOrchestrator(o)
	client := ipc.NewClient(path)
	waitForIPCState(t, client, Recording)
	response, err := client.Do(context.Background(), ipc.Cancel)
	if err != nil || response.State != string(Idle) {
		t.Fatalf("cancel response=%#v error=%v", response, err)
	}
	if result := awaitResult(t, done); result.Result != Cancelled {
		t.Fatalf("result=%#v", result)
	}
	stopServer()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapSocketRelaysStopBeforeAttach(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	bootstrap, err := NewBootstrap(BootstrapOptions{Context: context.Background(), SessionID: "bootstrap", Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shortTempDir(t), "session.sock")
	server, err := ipc.Listen(path, bootstrap, bus)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServer := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx) }()
	defer func() { stopServer(); <-serveDone }()
	client := ipc.NewClient(path)
	if response, err := client.Do(context.Background(), ipc.Status); err != nil || response.State != string(Starting) {
		t.Fatalf("status=%#v error=%v", response, err)
	}
	if response, err := client.Do(context.Background(), ipc.Stop); err != nil || response.State != string(Starting) {
		t.Fatalf("stop=%#v error=%v", response, err)
	}

	ready := make(chan struct{})
	machine, startedAt := bootstrap.machineOptions()
	runtimeDir := shortTempDir(t)
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: bootstrap.Context(), SessionID: "bootstrap", RuntimeDir: runtimeDir,
		Recorder:      &audio.FakeRecorder{FixturePath: sessionWAV(t), ReadyGate: ready},
		RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		Transcriber:   &fakeTranscriber{result: stt.MultiResult{Text: "raw", Provider: "primary", Segments: []stt.SegmentResult{{Provider: "primary"}}}},
		Processor:     &fakeProcessor{result: textpipe.Result{Text: "final"}}, Sink: &fakeSink{}, UI: ui.NewNone(), Bus: bus,
		Machine: machine, StartedAt: startedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.Attach(o)
	done := runOrchestrator(o)
	close(ready)
	if result := awaitResult(t, done); result.Result != Completed {
		t.Fatalf("result=%#v", result)
	}
}

func TestBootstrapCancelInterruptsConstructionContext(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	bootstrap, err := NewBootstrap(BootstrapOptions{Context: context.Background(), SessionID: "blocked", Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.setBuildingProvider("primary")
	stream, unsubscribe := bus.Subscribe(4)
	defer unsubscribe()
	response := bootstrap.Handle(context.Background(), ipc.NewRequest(ipc.Cancel))
	if !response.OK || response.State != string(Idle) {
		t.Fatalf("cancel=%#v", response)
	}
	select {
	case <-bootstrap.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("construction context not cancelled")
	}
	result := bootstrap.Finish(context.Canceled, "")
	if result.Result != Cancelled || result.Error != nil || result.Provider != "primary" {
		t.Fatalf("result=%#v", result)
	}
	select {
	case event := <-stream:
		if event.Type != events.Terminal || event.Result != string(Cancelled) || event.Provider != "primary" || event.ErrorClass != errs.Cancelled {
			t.Fatalf("terminal=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event missing")
	}
}

type orderedProcessor struct{ order *[]string }

func (p orderedProcessor) Run(_ context.Context, text string) (textpipe.Result, error) {
	*p.order = append(*p.order, "cleanup:"+text)
	return textpipe.Result{Text: "final"}, nil
}

type orderedClipboardSink struct{ order *[]string }

func (s orderedClipboardSink) CopyRaw(_ context.Context, text string) error {
	*s.order = append(*s.order, "raw:"+text)
	return nil
}

func (s orderedClipboardSink) Deliver(_ context.Context, text string) error {
	*s.order = append(*s.order, "deliver:"+text)
	return nil
}

func TestRawAndFinalClipboardRunAfterProcessingInOrderWithoutEventLeak(t *testing.T) {
	var order []string
	bus := events.NewBus()
	defer bus.Close()
	stream, unsubscribe := bus.Subscribe(32)
	defer unsubscribe()
	runtimeDir := t.TempDir()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "clipboard-order", RuntimeDir: runtimeDir,
		Recorder: &audio.FakeRecorder{FixturePath: sessionWAV(t)}, RecordOptions: audio.RecordOptions{Directory: runtimeDir},
		Transcriber: &fakeTranscriber{result: stt.MultiResult{Text: "brut-secret", Provider: "primary", Segments: []stt.SegmentResult{{Provider: "primary"}}}},
		Processor:   orderedProcessor{order: &order}, Sink: orderedClipboardSink{order: &order}, UI: ui.NewNone(), Bus: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
	if result := awaitResult(t, done); result.Result != Completed {
		t.Fatalf("result=%#v", result)
	}
	want := []string{"cleanup:brut-secret", "raw:brut-secret", "deliver:final"}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("order=%v", order)
	}
	for len(stream) > 0 {
		event := <-stream
		data, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(data), "brut-secret") || strings.Contains(string(data), "final") {
			t.Fatalf("transcript leaked in event: %s", data)
		}
	}
}

type trackingClipboardSink struct {
	mu       sync.Mutex
	calls    []string
	rawErr   error
	finalErr error
}

func (s *trackingClipboardSink) CopyRaw(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "raw:"+text)
	return s.rawErr
}

func (s *trackingClipboardSink) Deliver(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "final:"+text)
	return s.finalErr
}

func (s *trackingClipboardSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func TestProcessingWithoutDeliveryHasNoClipboardEffect(t *testing.T) {
	t.Run("cancel blocked processor", func(t *testing.T) {
		started := make(chan struct{})
		processor := &fakeProcessor{started: started, gate: make(chan struct{})}
		clipboard := &trackingClipboardSink{}
		o := newTestOrchestrator(t, nil, nil, processor, clipboard)
		done := runOrchestrator(o)
		waitForState(t, o, Recording)
		_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
		await(t, started)
		if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel)); !response.OK {
			t.Fatalf("cancel response=%#v", response)
		}
		if result := awaitResult(t, done); result.Result != Cancelled {
			t.Fatalf("result=%#v", result)
		}
		if calls := clipboard.snapshot(); len(calls) != 0 {
			t.Fatalf("clipboard calls=%v", calls)
		}
	})

	t.Run("empty processor result", func(t *testing.T) {
		clipboard := &trackingClipboardSink{}
		o := newTestOrchestrator(t, nil, nil, &fakeProcessor{result: textpipe.Result{Text: " \n"}}, clipboard)
		done := runOrchestrator(o)
		waitForState(t, o, Recording)
		_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
		if result := awaitResult(t, done); result.Result != NoSpeech {
			t.Fatalf("result=%#v", result)
		}
		if calls := clipboard.snapshot(); len(calls) != 0 {
			t.Fatalf("clipboard calls=%v", calls)
		}
	})

	t.Run("processor error", func(t *testing.T) {
		clipboard := &trackingClipboardSink{}
		processor := &fakeProcessor{err: errs.New(errs.ServerError, "processor", "refus")}
		o := newTestOrchestrator(t, nil, nil, processor, clipboard)
		done := runOrchestrator(o)
		waitForState(t, o, Recording)
		_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
		result := awaitResult(t, done)
		if result.Result != Failed || result.Error == nil || result.Error.Class != errs.ServerError {
			t.Fatalf("result=%#v", result)
		}
		if calls := clipboard.snapshot(); len(calls) != 0 {
			t.Fatalf("clipboard calls=%v", calls)
		}
	})
}

func TestClipboardCopyFailuresAreSinkFailures(t *testing.T) {
	tests := []struct {
		name     string
		rawErr   error
		finalErr error
		want     []string
	}{
		{name: "raw", rawErr: errs.New(errs.SinkFailed, "clipboard", "copie brute impossible"), want: []string{"raw:brut"}},
		{name: "final", finalErr: errs.New(errs.SinkFailed, "clipboard", "copie finale impossible"), want: []string{"raw:brut", "final:final"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clipboard := &trackingClipboardSink{rawErr: test.rawErr, finalErr: test.finalErr}
			o := newTestOrchestrator(t, nil, nil, nil, clipboard)
			done := runOrchestrator(o)
			waitForState(t, o, Recording)
			_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
			result := awaitResult(t, done)
			if result.Result != Failed || result.Error == nil || result.Error.Class != errs.SinkFailed {
				t.Fatalf("result=%#v", result)
			}
			if calls := clipboard.snapshot(); strings.Join(calls, "|") != strings.Join(test.want, "|") {
				t.Fatalf("clipboard calls=%v, want %v", calls, test.want)
			}
		})
	}
}

func TestTerminalEventCarriesEffectiveProviderAndFailureClass(t *testing.T) {
	o := newTestOrchestrator(t, nil, nil, nil, &fakeSink{err: errs.New(errs.SinkFailed, "sink", "refus")})
	stream, unsubscribe := o.bus.Subscribe(32)
	defer unsubscribe()
	done := runOrchestrator(o)
	waitForState(t, o, Recording)
	_ = o.Handle(context.Background(), ipc.NewRequest(ipc.Stop))
	_ = awaitResult(t, done)
	for len(stream) > 0 {
		event := <-stream
		if event.Type == events.Terminal {
			if event.Result != string(Failed) || event.Provider != "primary" || event.ErrorClass != errs.SinkFailed {
				t.Fatalf("terminal=%#v", event)
			}
			return
		}
	}
	t.Fatal("terminal event missing")
}

type sessionNotificationClient struct {
	terminal  chan struct{}
	release   chan struct{}
	closed    chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (c *sessionNotificationClient) Notify(ctx context.Context, _ uint32, notification ui.Notification) (uint32, error) {
	if notification.Category != "murmur.error" {
		return 1, nil
	}
	c.once.Do(func() { close(c.terminal) })
	select {
	case <-c.release:
		return 2, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (c *sessionNotificationClient) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestRunDrainsTerminalUIBeforeReturning(t *testing.T) {
	client := &sessionNotificationClient{
		terminal: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}),
	}
	notifications := ui.NewNotify(client, func(string, ...any) {})
	runtimeDir := t.TempDir()
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "terminal-ui", RuntimeDir: runtimeDir,
		Recorder:    &audio.FakeRecorder{StartErr: errs.New(errs.AudioFailed, "audio/fake", "refus")},
		Transcriber: &fakeTranscriber{}, Processor: &fakeProcessor{}, Sink: &fakeSink{},
		UI: notifications, Bus: events.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := runOrchestrator(o)
	await(t, client.terminal)
	select {
	case <-done:
		t.Fatal("Run returned before the terminal UI completed")
	default:
	}
	close(client.release)
	result := awaitResult(t, done)
	if result.Result != Failed {
		t.Fatalf("result = %#v", result)
	}
	await(t, client.closed)
}

func TestRunReportsPersistenceFailureSeparately(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), "missing")
	o, err := NewOrchestrator(OrchestratorOptions{
		Context: context.Background(), SessionID: "persistence-error", RuntimeDir: runtimeDir,
		Recorder: &audio.FakeRecorder{
			StartErr: errs.New(errs.AudioFailed, "audio/fake", "refus"),
		},
		Transcriber: &fakeTranscriber{}, Processor: &fakeProcessor{}, Sink: &fakeSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr, persistErr := o.Run()
	if result.Result != Failed || errs.Classify(runErr) != errs.AudioFailed {
		t.Fatalf("result=%#v runErr=%v", result, runErr)
	}
	if persistErr == nil || !errors.Is(persistErr, os.ErrNotExist) {
		t.Fatalf("persistErr=%v, want missing runtime", persistErr)
	}
}

type runOutcome struct {
	result     LastResult
	runErr     error
	persistErr error
}

func runOrchestrator(o *Orchestrator) <-chan runOutcome {
	done := make(chan runOutcome, 1)
	go func() {
		result, runErr, persistErr := o.Run()
		done <- runOutcome{result: result, runErr: runErr, persistErr: persistErr}
	}()
	return done
}

func awaitResult(t *testing.T, done <-chan runOutcome) LastResult {
	t.Helper()
	select {
	case outcome := <-done:
		if err := errors.Join(outcome.runErr, outcome.persistErr); err != nil && outcome.result.Result != Failed {
			t.Fatalf("Run error: %v", err)
		}
		return outcome.result
	case <-time.After(2 * time.Second):
		t.Fatal("session timeout")
		return LastResult{}
	}
}

func await(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("operation timeout")
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "murmur-session-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitForState(t *testing.T, o *Orchestrator, wanted State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if o.machine.State() == wanted {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state=%s, want %s", o.machine.State(), wanted)
}

func waitForIPCState(t *testing.T, client *ipc.Client, wanted State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Do(context.Background(), ipc.Status)
		if err == nil && response.State == string(wanted) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("IPC state never reached %s", wanted)
}

func sessionWAV(t *testing.T) string {
	return sessionWAVDuration(t, time.Second)
}

func sessionWAVDuration(t *testing.T, duration time.Duration) string {
	t.Helper()
	dataSize := uint32(int64(wav.ByteRate) * int64(duration) / int64(time.Second))
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], dataSize+36)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], wav.Channels)
	binary.LittleEndian.PutUint32(header[24:28], wav.SampleRate)
	binary.LittleEndian.PutUint32(header[28:32], wav.ByteRate)
	binary.LittleEndian.PutUint16(header[32:34], wav.BlockAlign)
	binary.LittleEndian.PutUint16(header[34:36], wav.BitsPerSample)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataSize)
	path := filepath.Join(t.TempDir(), "fixture.wav")
	if err := os.WriteFile(path, append(header, make([]byte, dataSize)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
