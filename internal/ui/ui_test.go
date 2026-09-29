package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/logging"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func event(kind events.Type, state string) events.Event {
	return events.Event{Version: 1, Type: kind, State: state, SessionID: "s1", Timestamp: time.Now()}
}

type notifyCall struct {
	replaces uint32
	value    Notification
}

type fakeNotificationClient struct {
	mu     sync.Mutex
	calls  []notifyCall
	called chan struct{}
	err    error
}

func (f *fakeNotificationClient) Notify(_ context.Context, replaces uint32, value Notification) (uint32, error) {
	f.mu.Lock()
	f.calls = append(f.calls, notifyCall{replaces: replaces, value: value})
	id := uint32(40 + len(f.calls))
	f.mu.Unlock()
	select {
	case f.called <- struct{}{}:
	default:
	}
	return id, f.err
}

func (f *fakeNotificationClient) Close() error { return nil }

func (f *fakeNotificationClient) Calls() []notifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notifyCall(nil), f.calls...)
}

func TestNotifyReplacesPreviousNotification(t *testing.T) {
	client := &fakeNotificationClient{called: make(chan struct{}, 2)}
	notify := NewNotify(client, func(string, ...any) {})
	notify.Show(event(events.SessionStarted, "starting"))
	waitSignal(t, client.called)
	notify.Show(event(events.CaptureReady, "recording"))
	waitSignal(t, client.called)
	notify.Close()

	calls := client.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].replaces != 0 || calls[1].replaces != 41 {
		t.Fatalf("replacement IDs = %d, %d", calls[0].replaces, calls[1].replaces)
	}
	if calls[1].value.TimeoutMS != 0 {
		t.Fatalf("recording timeout = %d", calls[1].value.TimeoutMS)
	}
}

func TestNotifyFailureIsBestEffort(t *testing.T) {
	client := &fakeNotificationClient{called: make(chan struct{}, 1), err: errors.New("no bus")}
	notify := NewNotify(client, func(string, ...any) {})
	notify.Show(event(events.CaptureReady, "recording"))
	waitSignal(t, client.called)
	notify.Close()
}

func TestUIOutagesUseClosedOperationalMessages(t *testing.T) {
	const canary = "MURMUR-UI-CAUSE-CANARY"
	for _, test := range []struct {
		name      string
		component string
		run       func(*testing.T)
	}{
		{
			name: "dbus", component: "dbus",
			run: func(t *testing.T) {
				client := &fakeNotificationClient{called: make(chan struct{}, 1), err: errors.New(canary)}
				notify := NewNotify(client, nil)
				notify.Show(event(events.CaptureReady, "recording"))
				waitSignal(t, client.called)
				notify.Close()
			},
		},
		{
			name: "osd", component: "osd",
			run: func(t *testing.T) {
				started := make(chan struct{}, 1)
				executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
					started <- struct{}{}
					return nil, errors.New(canary)
				}}
				osd := NewOSD(executor, OSDOptions{MaxRestarts: -1})
				osd.Show(event(events.CaptureReady, "recording"))
				waitSignal(t, started)
				osd.Close()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "murmur.log")
			if err := logging.ConfigureFile("warn", path, 4096, 2); err != nil {
				t.Fatal(err)
			}
			test.run(t)
			if err := logging.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"component":"`+test.component+`"`) || strings.Contains(string(data), canary) {
				t.Fatalf("operational log = %s", data)
			}
		})
	}
}

type drainingNotificationClient struct {
	activeStarted chan struct{}
	terminal      chan struct{}
	release       chan struct{}
	closed        chan struct{}
	activeOnce    sync.Once
	terminalOnce  sync.Once
	closeOnce     sync.Once
}

func (c *drainingNotificationClient) Notify(ctx context.Context, _ uint32, value Notification) (uint32, error) {
	if value.Category == "murmur.success" {
		c.terminalOnce.Do(func() { close(c.terminal) })
		select {
		case <-c.release:
			return 42, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	c.activeOnce.Do(func() { close(c.activeStarted) })
	<-ctx.Done()
	return 0, ctx.Err()
}

func (c *drainingNotificationClient) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestNotifyCloseCancelsActiveStateAndDrainsTerminal(t *testing.T) {
	client := &drainingNotificationClient{
		activeStarted: make(chan struct{}),
		terminal:      make(chan struct{}),
		release:       make(chan struct{}),
		closed:        make(chan struct{}),
	}
	notify := newNotify(client, func(string, ...any) {}, time.Second, 200*time.Millisecond)
	notify.Show(event(events.CaptureReady, "recording"))
	waitSignal(t, client.activeStarted)
	terminal := event(events.Terminal, "idle")
	terminal.Result = "completed"
	notify.Show(terminal)
	closed := make(chan struct{})
	go func() {
		notify.Close()
		close(closed)
	}()
	waitSignal(t, client.terminal)
	select {
	case <-closed:
		t.Fatal("Close returned before the terminal notification completed")
	default:
	}
	close(client.release)
	waitSignal(t, closed)
	waitSignal(t, client.closed)
}

func TestNotifyCloseWithoutTerminalCancelsImmediately(t *testing.T) {
	client := &drainingNotificationClient{
		activeStarted: make(chan struct{}), terminal: make(chan struct{}),
		release: make(chan struct{}), closed: make(chan struct{}),
	}
	notify := newNotify(client, func(string, ...any) {}, time.Second, 200*time.Millisecond)
	notify.Show(event(events.CaptureReady, "recording"))
	waitSignal(t, client.activeStarted)
	closed := make(chan struct{})
	go func() {
		notify.Close()
		close(closed)
	}()
	waitSignal(t, closed)
	waitSignal(t, client.closed)
}

func TestNotifyTerminalCloseStopsAtBudget(t *testing.T) {
	client := &drainingNotificationClient{
		activeStarted: make(chan struct{}), terminal: make(chan struct{}),
		release: make(chan struct{}), closed: make(chan struct{}),
	}
	notify := newNotify(client, func(string, ...any) {}, time.Second, 20*time.Millisecond)
	terminal := event(events.Terminal, "idle")
	terminal.Result = "completed"
	notify.Show(terminal)
	closed := make(chan struct{})
	go func() {
		notify.Close()
		close(closed)
	}()
	waitSignal(t, client.terminal)
	waitSignal(t, closed)
	waitSignal(t, client.closed)
}

func TestTerminalCancellationAndNoSpeechAreNotFailureNotifications(t *testing.T) {
	for _, result := range []struct {
		name  string
		class errs.Class
	}{
		{name: "cancelled", class: errs.Cancelled},
		{name: "no_speech", class: errs.NoSpeech},
	} {
		event := event(events.Terminal, "idle")
		event.Result = result.name
		event.ErrorClass = result.class
		notification, ok := notificationForEvent(event)
		if !ok || notification.Urgency == 2 {
			t.Fatalf("result=%s notification=%#v", result.name, notification)
		}
	}
}

func TestNotificationVisualCategories(t *testing.T) {
	tests := []struct {
		name     string
		event    events.Event
		category string
		body     string
		summary  string
	}{
		{name: "starting", event: event(events.SessionStarted, "starting"), category: "murmur.starting", body: "Démarrage de la capture", summary: "🟢 Murmur"},
		{name: "recording", event: event(events.CaptureReady, "recording"), category: "murmur.recording", body: "Écoute en cours", summary: "🟢 Murmur"},
		{name: "processing", event: event(events.StateChanged, "transcribing"), category: "murmur.processing", body: "Transcription en cours", summary: "🟢 Murmur"},
		{name: "degraded", event: event(events.Degraded, "error"), category: "murmur.warning", body: "Fonctionnement dégradé", summary: "🔴 Murmur"},
		{name: "success", event: events.Event{Type: events.Terminal, Result: "completed"}, category: "murmur.success", body: "Dictée terminée", summary: "🟢 Murmur"},
		{name: "cancelled", event: events.Event{Type: events.Terminal, Result: "cancelled"}, category: "murmur.cancelled", body: "Dictée annulée", summary: "🟠 Murmur"},
		{name: "error", event: events.Event{Type: events.Terminal, Result: "failed"}, category: "murmur.error", body: "Échec de la dictée", summary: "🔴 Murmur"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			notification, ok := notificationForEvent(test.event)
			if !ok || notification.Category != test.category || notification.Summary != test.summary || notification.Body != test.body {
				t.Fatalf("notification = %#v", notification)
			}
		})
	}
}

func TestOSDFailureIsBestEffort(t *testing.T) {
	started := make(chan struct{}, 1)
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		started <- struct{}{}
		return nil, errors.New("OSD unavailable")
	}}
	osd := NewOSD(executor, OSDOptions{MaxRestarts: -1, Warn: func(string, ...any) {}})
	for i := 0; i < 100; i++ {
		osd.Show(event(events.StateChanged, "transcribing"))
	}
	waitSignal(t, started)
	osd.Close()
	if calls := executor.Calls(); len(calls) != 1 {
		t.Fatalf("start attempts = %d, want bounded to 1", len(calls))
	}
}

type inputProcess struct {
	stdin    *recordingInput
	done     chan struct{}
	once     sync.Once
	killed   chan struct{}
	waited   chan struct{}
	waitOnce sync.Once
}

func newInputProcess() *inputProcess {
	return &inputProcess{
		stdin:  newRecordingInput(),
		done:   make(chan struct{}),
		killed: make(chan struct{}, 1),
		waited: make(chan struct{}),
	}
}

func (p *inputProcess) Stdin() io.WriteCloser  { return p.stdin }
func (p *inputProcess) Signal(os.Signal) error { return nil }
func (p *inputProcess) Wait() error {
	<-p.done
	p.waitOnce.Do(func() { close(p.waited) })
	return errors.New("helper exited")
}
func (p *inputProcess) Kill() error {
	p.Exit()
	select {
	case p.killed <- struct{}{}:
	default:
	}
	return nil
}
func (p *inputProcess) Exit() {
	p.stdin.Close()
	p.once.Do(func() { close(p.done) })
}

type recordingInput struct {
	mu           sync.Mutex
	buffer       bytes.Buffer
	closed       bool
	wrote        chan struct{}
	closedSignal chan struct{}
	closeOnce    sync.Once
}

func newRecordingInput() *recordingInput {
	return &recordingInput{wrote: make(chan struct{}, 8), closedSignal: make(chan struct{})}
}

func (w *recordingInput) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := w.buffer.Write(value)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return n, err
}
func (w *recordingInput) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.closeOnce.Do(func() { close(w.closedSignal) })
	return nil
}
func (w *recordingInput) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buffer.Bytes()...)
}

func TestOSDJSONLRestartAndClose(t *testing.T) {
	first := newInputProcess()
	second := newInputProcess()
	var mu sync.Mutex
	starts := 0
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		mu.Lock()
		defer mu.Unlock()
		starts++
		if starts == 1 {
			return first, nil
		}
		return second, nil
	}}
	osd := NewOSD(executor, OSDOptions{
		Buffer: 2, MaxRestarts: 1, IdleCloseTimeout: 20 * time.Millisecond,
		Warn: func(string, ...any) {},
	})
	osd.Show(event(events.CaptureReady, "recording"))
	waitSignal(t, first.stdin.wrote)

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(first.stdin.Bytes()), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["v"] != float64(1) || payload["state"] != "recording" {
		t.Fatalf("payload = %#v", payload)
	}
	for _, forbidden := range []string{"text", "body", "transcript"} {
		if _, found := payload[forbidden]; found {
			t.Fatalf("forbidden field %q in payload", forbidden)
		}
	}

	first.Exit()
	waitSignal(t, second.stdin.wrote)
	osd.Close()
	waitSignal(t, second.killed)
	waitSignal(t, second.waited)
}

func TestOSDCloseWritesTerminalThenWaitsForHelper(t *testing.T) {
	process := newInputProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return process, nil
	}}
	osd := NewOSD(executor, OSDOptions{CloseTimeout: 200 * time.Millisecond, Warn: func(string, ...any) {}})
	terminal := event(events.Terminal, "idle")
	terminal.Result = "completed"
	osd.Show(terminal)
	closed := make(chan struct{})
	go func() {
		osd.Close()
		close(closed)
	}()
	waitSignal(t, process.stdin.wrote)
	waitSignal(t, process.stdin.closedSignal)

	var payload OSDPayload
	if err := json.Unmarshal(bytes.TrimSpace(process.stdin.Bytes()), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.State != "terminal" || payload.Result != "completed" {
		t.Fatalf("payload = %#v", payload)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before the helper terminal timeout")
	case <-process.killed:
		t.Fatal("helper killed before its terminal timeout")
	default:
	}
	process.Exit()
	waitSignal(t, closed)
	waitSignal(t, process.waited)
	if calls := executor.Calls(); len(calls) != 1 {
		t.Fatalf("start calls = %d, want 1", len(calls))
	}
}

func TestOSDShutdownRestartsTerminalAfterSuccessfulWriteThenExit(t *testing.T) {
	exited := newInputProcess()
	terminal := OSDPayload{Version: 1, State: "terminal", Visible: true, Result: "completed"}
	if err := writeOSDPayload(exited, terminal); err != nil {
		t.Fatal(err)
	}
	exited.Exit()
	exitedDone := make(chan error, 1)
	exitedDone <- exited.Wait()

	replacement := newInputProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return replacement, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	osd := &OSD{
		executor:        executor,
		options:         OSDOptions{Helper: DefaultOSDHelper, Warn: func(string, ...any) {}},
		ctx:             ctx,
		cancel:          cancel,
		proc:            exited,
		terminal:        &terminal,
		terminalWritten: true,
		closeDeadline:   time.Now().Add(time.Second),
	}
	go func() {
		<-replacement.stdin.closedSignal
		replacement.Exit()
	}()
	osd.shutdown(exitedDone)

	waitSignal(t, replacement.stdin.wrote)
	waitSignal(t, replacement.waited)
	var payload OSDPayload
	if err := json.Unmarshal(bytes.TrimSpace(replacement.stdin.Bytes()), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.State != "terminal" || payload.Result != "completed" {
		t.Fatalf("payload = %#v", payload)
	}
	if calls := executor.Calls(); len(calls) != 1 {
		t.Fatalf("restart calls = %d, want 1", len(calls))
	}
}

func TestOSDShutdownRestartsTerminalAfterStaleHelperWriteFailure(t *testing.T) {
	stale := newInputProcess()
	if err := stale.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	staleDone := make(chan error, 1)
	go func() { staleDone <- stale.Wait() }()

	replacement := newInputProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return replacement, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	terminal := OSDPayload{Version: 1, State: "terminal", Visible: true, Result: "completed"}
	osd := &OSD{
		executor:      executor,
		options:       OSDOptions{Helper: DefaultOSDHelper, Warn: func(string, ...any) {}},
		ctx:           ctx,
		cancel:        cancel,
		proc:          stale,
		terminal:      &terminal,
		closeDeadline: time.Now().Add(time.Second),
	}
	go func() {
		<-replacement.stdin.closedSignal
		replacement.Exit()
	}()
	osd.shutdown(staleDone)

	waitSignal(t, stale.killed)
	waitSignal(t, stale.waited)
	waitSignal(t, replacement.stdin.wrote)
	waitSignal(t, replacement.waited)
	var payload OSDPayload
	if err := json.Unmarshal(bytes.TrimSpace(replacement.stdin.Bytes()), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.State != "terminal" || payload.Result != "completed" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestOSDCloseWithoutTerminalUsesShortBudget(t *testing.T) {
	process := newInputProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return process, nil
	}}
	osd := NewOSD(executor, OSDOptions{IdleCloseTimeout: 20 * time.Millisecond, Warn: func(string, ...any) {}})
	osd.Show(event(events.CaptureReady, "recording"))
	waitSignal(t, process.stdin.wrote)
	closed := make(chan struct{})
	go func() {
		osd.Close()
		close(closed)
	}()
	waitSignal(t, process.stdin.closedSignal)
	waitSignal(t, process.killed)
	waitSignal(t, closed)
	waitSignal(t, process.waited)
}

func TestOSDTerminalCloseKillsAndReapsAtBudget(t *testing.T) {
	process := newInputProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return process, nil
	}}
	osd := NewOSD(executor, OSDOptions{CloseTimeout: 20 * time.Millisecond, Warn: func(string, ...any) {}})
	terminal := event(events.Terminal, "idle")
	terminal.Result = "failed"
	osd.Show(terminal)
	closed := make(chan struct{})
	go func() {
		osd.Close()
		close(closed)
	}()
	waitSignal(t, process.stdin.wrote)
	waitSignal(t, process.stdin.closedSignal)
	waitSignal(t, process.killed)
	waitSignal(t, closed)
	waitSignal(t, process.waited)
}

type blockingInput struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (w *blockingInput) Write(value []byte) (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockingInput) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

type blockingInputProcess struct {
	stdin    *blockingInput
	done     chan struct{}
	killed   chan struct{}
	waited   chan struct{}
	killOnce sync.Once
	waitOnce sync.Once
}

func newBlockingInputProcess() *blockingInputProcess {
	return &blockingInputProcess{
		stdin: &blockingInput{started: make(chan struct{}), closed: make(chan struct{})},
		done:  make(chan struct{}), killed: make(chan struct{}), waited: make(chan struct{}),
	}
}

func (p *blockingInputProcess) Stdin() io.WriteCloser  { return p.stdin }
func (p *blockingInputProcess) Signal(os.Signal) error { return nil }
func (p *blockingInputProcess) Wait() error {
	<-p.done
	p.waitOnce.Do(func() { close(p.waited) })
	return nil
}
func (p *blockingInputProcess) Kill() error {
	p.killOnce.Do(func() {
		_ = p.stdin.Close()
		close(p.killed)
		close(p.done)
	})
	return nil
}

func TestOSDTerminalBlockedWriteIsClosedKilledAndReaped(t *testing.T) {
	process := newBlockingInputProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return process, nil
	}}
	osd := NewOSD(executor, OSDOptions{CloseTimeout: 20 * time.Millisecond, Warn: func(string, ...any) {}})
	terminal := event(events.Terminal, "idle")
	terminal.Result = "completed"
	osd.Show(terminal)
	waitSignal(t, process.stdin.started)
	closed := make(chan struct{})
	go func() {
		osd.Close()
		close(closed)
	}()
	waitSignal(t, process.stdin.closed)
	waitSignal(t, process.killed)
	waitSignal(t, process.waited)
	waitSignal(t, closed)
}

func TestOSDShowDoesNotBlockOnSlowHelper(t *testing.T) {
	process := newInputProcess()
	block := make(chan struct{})
	process.stdin = newRecordingInput()
	process.stdin.wrote = block
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return process, nil
	}}
	osd := NewOSD(executor, OSDOptions{Buffer: 1, IdleCloseTimeout: 20 * time.Millisecond})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			osd.Show(event(events.StateChanged, "processing"))
		}
		close(done)
	}()
	waitSignal(t, done)
	osd.Close()
}

func TestSoundPlaysWhenCaptureStartsAndStops(t *testing.T) {
	called := make(chan struct{}, 4)
	executor := &sysexec.FakeExecutor{RunFunc: func(_ context.Context, name string, _ []string, _ []byte) ([]byte, error) {
		called <- struct{}{}
		if name == "pw-play" {
			return nil, errors.New("missing")
		}
		return nil, nil
	}}
	sound := NewSound(NewNone(), executor, "/sound.oga", func(string, ...any) {})
	sound.Show(event(events.CaptureReady, "recording"))
	waitSignal(t, called)
	waitSignal(t, called)
	sound.Show(event(events.StateChanged, "stopping"))
	waitSignal(t, called)
	waitSignal(t, called)
	sound.Show(events.Event{Type: events.Terminal, Result: "completed"})
	sound.Close()
	calls := executor.Calls()
	if len(calls) != 4 || calls[0].Name != "pw-play" || calls[1].Name != "paplay" ||
		calls[2].Name != "pw-play" || calls[3].Name != "paplay" {
		t.Fatalf("sound calls = %#v", calls)
	}
}

func TestSoundPlaysWhenActiveCaptureIsCancelled(t *testing.T) {
	executor := &sysexec.FakeExecutor{RunFunc: func(context.Context, string, []string, []byte) ([]byte, error) {
		return nil, nil
	}}
	sound := NewSound(NewNone(), executor, "/sound.oga", func(string, ...any) {})
	sound.Show(event(events.CaptureReady, "recording"))
	sound.Show(events.Event{Type: events.Terminal, Result: "cancelled"})
	sound.Close()
	if calls := executor.Calls(); len(calls) != 2 {
		t.Fatalf("sound calls = %#v", calls)
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}
