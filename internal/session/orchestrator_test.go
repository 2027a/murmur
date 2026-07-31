package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/ipc"
	"git.2027a.net/mathieu/murmur/internal/stt"
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

func (f *fakeTranscriber) TranscribeSegments(ctx context.Context, _ []stt.Segment) (stt.MultiResult, error) {
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
				return f.TranscribeSegments(ctx, nil)
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

func newTestOrchestrator(t *testing.T, ready <-chan struct{}, transcriber Transcriber, processor Processor, target *fakeSink) *Orchestrator {
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

func TestFakeIntegrationStartStatusCancelCycle(t *testing.T) {
	o := newTestOrchestrator(t, nil, nil, nil, &fakeSink{})
	if err := o.Begin(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.sock")
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
	path := filepath.Join(t.TempDir(), "session.sock")
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
	runtimeDir := t.TempDir()
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

func TestRawClipboardRunsBetweenSTTAndCleanupWithoutEventLeak(t *testing.T) {
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
	want := []string{"raw:brut-secret", "cleanup:brut-secret", "deliver:final"}
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

type runOutcome struct {
	result LastResult
	err    error
}

func runOrchestrator(o *Orchestrator) <-chan runOutcome {
	done := make(chan runOutcome, 1)
	go func() {
		result, err := o.Run()
		done <- runOutcome{result: result, err: err}
	}()
	return done
}

func awaitResult(t *testing.T, done <-chan runOutcome) LastResult {
	t.Helper()
	select {
	case outcome := <-done:
		if outcome.err != nil && outcome.result.Result != Failed {
			t.Fatalf("Run error: %v", outcome.err)
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
	t.Helper()
	dataSize := uint32(wav.ByteRate)
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
