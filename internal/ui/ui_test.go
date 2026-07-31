package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
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
	stdin  *recordingInput
	done   chan struct{}
	once   sync.Once
	killed chan struct{}
}

func newInputProcess() *inputProcess {
	return &inputProcess{
		stdin:  &recordingInput{wrote: make(chan struct{}, 8)},
		done:   make(chan struct{}),
		killed: make(chan struct{}, 1),
	}
}

func (p *inputProcess) Stdin() io.WriteCloser  { return p.stdin }
func (p *inputProcess) Signal(os.Signal) error { return nil }
func (p *inputProcess) Wait() error {
	<-p.done
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
	mu     sync.Mutex
	buffer bytes.Buffer
	closed bool
	wrote  chan struct{}
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
	osd := NewOSD(executor, OSDOptions{Buffer: 2, MaxRestarts: 1, Warn: func(string, ...any) {}})
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
}

func TestOSDShowDoesNotBlockOnSlowHelper(t *testing.T) {
	process := newInputProcess()
	block := make(chan struct{})
	process.stdin = &recordingInput{wrote: block}
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return process, nil
	}}
	osd := NewOSD(executor, OSDOptions{Buffer: 1})
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

func TestSoundCascadeStopsOnSuccess(t *testing.T) {
	called := make(chan struct{}, 3)
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
	sound.Close()
	calls := executor.Calls()
	if len(calls) != 2 || calls[0].Name != "pw-play" || calls[1].Name != "paplay" {
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
