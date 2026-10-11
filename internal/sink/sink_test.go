package sink

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

type call struct {
	name  string
	args  []string
	stdin string
}

type fakeExecutor struct {
	sysexec.OSExecutor
	mu      sync.Mutex
	calls   []call
	results []error
}

func (f *fakeExecutor) Run(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	if len(f.results) == 0 {
		return nil, nil
	}
	err := f.results[0]
	f.results = f.results[1:]
	return nil, err
}

func (f *fakeExecutor) RunDiscard(ctx context.Context, name string, args []string, stdin []byte) error {
	_, err := f.Run(ctx, name, args, stdin)
	return err
}

func sinkConfig(mode, clipboardMode string) config.Config {
	cfg := config.Defaults()
	cfg.Sink.Mode = mode
	cfg.Sink.ClipboardMode = clipboardMode
	return cfg
}

func TestPasteSinkCommandSequence(t *testing.T) {
	executor := &fakeExecutor{}
	delivery, err := New(sinkConfig("paste", "clean-only"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "final"); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{name: "wl-copy", stdin: "final"},
		{name: "wtype", args: []string{"-M", "ctrl", "v", "-m", "ctrl"}},
	}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, want)
	}
}

func TestTypeSinkWithoutClipboard(t *testing.T) {
	executor := &fakeExecutor{}
	delivery, err := New(sinkConfig("type", "none"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "one\ntwo"); err != nil {
		t.Fatal(err)
	}
	want := []call{{name: "wtype", args: []string{"-"}, stdin: "one two"}}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, want)
	}
}

func TestActiveSinksSanitizeProviderText(t *testing.T) {
	const input = "a\r\nb\nc\rd\te\x1bf\x00g\x7fh\u0085i\u009fjé🙂"
	const safe = "a b c d efghijé🙂"

	t.Run("type clipboard and wtype", func(t *testing.T) {
		executor := &fakeExecutor{}
		delivery, err := New(sinkConfig("type", "clean-only"), Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		want := []call{
			{name: "wl-copy", stdin: safe},
			{name: "wtype", args: []string{"-"}, stdin: safe},
		}
		if !reflect.DeepEqual(executor.calls, want) {
			t.Fatalf("calls = %#v, want %#v", executor.calls, want)
		}
	})

	t.Run("dangerous keep retains only line feeds", func(t *testing.T) {
		executor := &fakeExecutor{}
		cfg := sinkConfig("type", "clean-only")
		cfg.Sink.TypeNewlines = "dangerous-keep"
		delivery, err := New(cfg, Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		const dangerous = "a\nb\nc\nd efghijé🙂"
		want := []call{
			{name: "wl-copy", stdin: dangerous},
			{name: "wtype", args: []string{"-"}, stdin: dangerous},
		}
		if !reflect.DeepEqual(executor.calls, want) {
			t.Fatalf("calls = %#v, want %#v", executor.calls, want)
		}
	})

	t.Run("paste shortcut failure stops before fallback", func(t *testing.T) {
		failure := errors.New("shortcut failed")
		executor := &fakeExecutor{results: []error{nil, failure}}
		cfg := sinkConfig("paste", "clean-only")
		cfg.Sink.PasteShortcuts = []string{"ctrl+v"}
		delivery, err := New(cfg, Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), input); !errors.Is(err, failure) || errs.Classify(err) != errs.SinkFailed {
			t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
		}
		want := []call{
			{name: "wl-copy", stdin: safe},
			{name: "wtype", args: []string{"-M", "ctrl", "v", "-m", "ctrl"}},
		}
		if !reflect.DeepEqual(executor.calls, want) {
			t.Fatalf("calls = %#v, want %#v", executor.calls, want)
		}
	})

	for _, mode := range []string{"paste", "auto"} {
		t.Run(mode+" without clipboard", func(t *testing.T) {
			executor := &fakeExecutor{}
			cfg := sinkConfig(mode, "none")
			cfg.Sink.AutoMode = "paste"
			delivery, err := New(cfg, Options{Executor: executor})
			if err != nil {
				t.Fatal(err)
			}
			if err := delivery.Deliver(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			want := []call{{name: "wtype", args: []string{"-"}, stdin: safe}}
			if !reflect.DeepEqual(executor.calls, want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, want)
			}
		})
	}
}

func TestActiveRawClipboardUsesSanitizedText(t *testing.T) {
	executor := &fakeExecutor{}
	cfg := sinkConfig("paste", "raw-and-clean")
	delivery, err := New(cfg, Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	raw := delivery.(interface {
		CopyRaw(context.Context, string) error
	})
	if err := raw.CopyRaw(context.Background(), "raw\n\x1b\u0085text"); err != nil {
		t.Fatal(err)
	}
	if got := executor.calls[0].stdin; got != "raw text" {
		t.Fatalf("raw wl-copy stdin = %q", got)
	}
}

func TestPassiveSinksPreserveProviderText(t *testing.T) {
	const input = "a\r\nb\tc\x1b\x00\u0085é🙂"

	t.Run("clipboard", func(t *testing.T) {
		executor := &fakeExecutor{}
		delivery, err := New(sinkConfig("clipboard", "clean-only"), Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if len(executor.calls) != 1 || executor.calls[0].stdin != input {
			t.Fatalf("calls = %#v", executor.calls)
		}
	})

	t.Run("clipboard-only", func(t *testing.T) {
		executor := &fakeExecutor{}
		delivery, err := New(sinkConfig("paste", "clean-only"), Options{Executor: executor, ClipboardOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if len(executor.calls) != 1 || executor.calls[0].stdin != input {
			t.Fatalf("calls = %#v", executor.calls)
		}
	})

	t.Run("stdout", func(t *testing.T) {
		var output strings.Builder
		delivery, err := New(sinkConfig("stdout", "none"), Options{Stdout: &output})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if output.String() != input+"\n" {
			t.Fatalf("stdout = %q", output.String())
		}
	})

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "result.txt")
		cfg := sinkConfig("file", "none")
		cfg.Sink.File = path
		delivery, err := New(cfg, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != input {
			t.Fatalf("file = %q, error=%v", data, err)
		}
	})
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	return max(0, len(p)-1), nil
}

func TestStdoutSinkDetectsShortWriteOfTextPlusLineFeed(t *testing.T) {
	delivery, err := New(sinkConfig("stdout", "none"), Options{Stdout: shortWriter{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "final"); errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
}

func TestClipboardSink(t *testing.T) {
	executor := &fakeExecutor{}
	delivery, err := New(sinkConfig("clipboard", "clean-only"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "final"); err != nil {
		t.Fatal(err)
	}
	want := []call{{name: "wl-copy", stdin: "final"}}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, want)
	}
}

func TestStdoutAndAtomicFileSinksNeedNoDesktopTools(t *testing.T) {
	t.Run("stdout", func(t *testing.T) {
		var output strings.Builder
		delivery, err := New(sinkConfig("stdout", "none"), Options{Stdout: &output})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), "final"); err != nil {
			t.Fatal(err)
		}
		if output.String() != "final\n" {
			t.Fatalf("stdout = %q", output.String())
		}
	})

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "result.txt")
		cfg := sinkConfig("file", "none")
		cfg.Sink.File = path
		delivery, err := New(cfg, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), "final"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "final" {
			t.Fatalf("file = %q", data)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 600", info.Mode().Perm())
		}
	})
}

func TestAutoResolvesToConfiguredTarget(t *testing.T) {
	executor := &fakeExecutor{}
	cfg := sinkConfig("auto", "none")
	cfg.General.Profile = "local"
	cfg.Sink.AutoMode = "type"
	delivery, err := New(cfg, Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "final"); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || !reflect.DeepEqual(executor.calls[0].args, []string{"-"}) {
		t.Fatalf("calls = %#v", executor.calls)
	}
}

func TestTransientPasteShortcutIsRejectedDefensively(t *testing.T) {
	for _, mode := range []string{"paste", "auto"} {
		t.Run(mode, func(t *testing.T) {
			cfg := sinkConfig(mode, "transient")
			cfg.Sink.AutoMode = "paste"
			_, err := New(cfg, Options{Executor: &fakeExecutor{}})
			if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "consommation") {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
}

func TestTransientDirectTypingClearsAfterInjection(t *testing.T) {
	for _, mode := range []string{"type", "paste"} {
		t.Run(mode, func(t *testing.T) {
			executor := &fakeExecutor{}
			cfg := sinkConfig(mode, "transient")
			if mode == "paste" {
				cfg.Sink.PasteShortcuts = nil
			}
			delivery, err := New(cfg, Options{Executor: executor})
			if err != nil {
				t.Fatal(err)
			}
			if err := delivery.Deliver(context.Background(), "final"); err != nil {
				t.Fatal(err)
			}
			want := []call{
				{name: "wl-copy", stdin: "final"},
				{name: "wtype", args: []string{"-"}, stdin: "final"},
				{name: "wl-copy", args: []string{"--clear"}},
			}
			if !reflect.DeepEqual(executor.calls, want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, want)
			}
		})
	}
}

func TestClipboardOnlyOverridesPasteBeforeInjection(t *testing.T) {
	executor := &fakeExecutor{}
	delivery, err := New(sinkConfig("paste", "clean-only"), Options{Executor: executor, ClipboardOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "final"); err != nil {
		t.Fatal(err)
	}
	want := []call{{name: "wl-copy", stdin: "final"}}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, want)
	}
}

func TestClipboardOnlyRejectsNonPersistentPolicy(t *testing.T) {
	for _, mode := range []string{"none", "transient"} {
		_, err := New(sinkConfig("paste", mode), Options{ClipboardOnly: true})
		if errs.Classify(err) != errs.ConfigInvalid {
			t.Fatalf("mode %s error = %v", mode, err)
		}
	}
}

func TestNewReturnsComposedSinkWithoutAtMostOnceGuard(t *testing.T) {
	var output strings.Builder
	delivery, err := New(sinkConfig("stdout", "none"), Options{Stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "first\nsecond\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestFileWithoutPathIsConfigInvalid(t *testing.T) {
	_, err := New(sinkConfig("file", "none"), Options{})
	if errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("error = %v", err)
	}
}

type countingSink struct {
	calls atomic.Int32
	err   error
	gate  chan struct{}
}

func (s *countingSink) Deliver(context.Context, string) error {
	s.calls.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	return s.err
}

func TestOnceDeliversAtMostOnceUnderConcurrency(t *testing.T) {
	wantErr := errors.New("delivery failed")
	target := &countingSink{err: wantErr, gate: make(chan struct{})}
	delivery := Once(target)

	const callers = 32
	start := make(chan struct{})
	errorsSeen := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			errorsSeen <- delivery.Deliver(context.Background(), "final")
		}()
	}
	ready.Wait()
	close(start)
	for target.calls.Load() == 0 {
	}
	close(target.gate)
	for range callers {
		if err := <-errorsSeen; !errors.Is(err, wantErr) {
			t.Fatalf("error = %v", err)
		}
	}
	if got := target.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestSinkFailurePreservesClipboardAccordingToPolicy(t *testing.T) {
	failure := errors.New("wtype failed")

	t.Run("persistent", func(t *testing.T) {
		executor := &fakeExecutor{results: []error{nil, failure, failure, failure, failure}}
		delivery, err := New(sinkConfig("paste", "clean-only"), Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), "final"); errs.Classify(err) != errs.SinkFailed {
			t.Fatalf("error = %v", err)
		}
		for _, call := range executor.calls {
			if reflect.DeepEqual(call.args, []string{"--clear"}) {
				t.Fatal("persistent clipboard was cleared")
			}
		}
		if executor.calls[0].name != "wl-copy" || executor.calls[0].stdin != "final" {
			t.Fatalf("calls = %#v", executor.calls)
		}
	})

	t.Run("transient type", func(t *testing.T) {
		executor := &fakeExecutor{results: []error{nil, failure, nil}}
		delivery, err := New(sinkConfig("type", "transient"), Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), "final"); errs.Classify(err) != errs.SinkFailed {
			t.Fatalf("error = %v", err)
		}
		want := []call{
			{name: "wl-copy", stdin: "final"},
			{name: "wtype", args: []string{"-"}, stdin: "final"},
			{name: "wl-copy", args: []string{"--clear"}},
		}
		if !reflect.DeepEqual(executor.calls, want) {
			t.Fatalf("calls = %#v, want %#v", executor.calls, want)
		}
	})
}

func TestTransientClearRunsOnceAfterFinalCopyFailureAndJoinsErrors(t *testing.T) {
	copyFailure := errors.New("copy final failed")
	clearFailure := errors.New("clear failed")
	executor := &fakeExecutor{results: []error{copyFailure, clearFailure}}
	delivery, err := New(sinkConfig("type", "transient"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	err = delivery.Deliver(context.Background(), "final")
	if errs.Classify(err) != errs.SinkFailed || !errors.Is(err, copyFailure) || !errors.Is(err, clearFailure) {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	want := []call{
		{name: "wl-copy", stdin: "final"},
		{name: "wl-copy", args: []string{"--clear"}},
	}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, want)
	}
}

func TestTransientClearGetsFreshBudgetAfterInjectionDeadline(t *testing.T) {
	var calls int
	executor := &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, _ string, args []string, _ []byte) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			return nil, nil
		case 2:
			<-ctx.Done()
			return nil, ctx.Err()
		case 3:
			if !reflect.DeepEqual(args, []string{"--clear"}) {
				t.Fatalf("clear args = %v", args)
			}
			if err := ctx.Err(); err != nil {
				t.Fatalf("clear context = %v", err)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("clear without deadline")
			}
			return nil, nil
		default:
			t.Fatalf("unexpected call %d", calls)
			return nil, nil
		}
	}}
	delivery, err := New(sinkConfig("type", "transient"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := delivery.Deliver(ctx, "final"); errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestTransientClearStillObservesExplicitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	executor := &sysexec.FakeExecutor{RunFunc: func(commandCtx context.Context, _ string, args []string, _ []byte) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			return nil, nil
		case 2:
			cancel()
			return nil, context.Canceled
		case 3:
			if !reflect.DeepEqual(args, []string{"--clear"}) {
				t.Fatalf("clear args = %v", args)
			}
			if !errors.Is(commandCtx.Err(), context.Canceled) {
				t.Fatalf("clear context = %v", commandCtx.Err())
			}
			return nil, commandCtx.Err()
		default:
			t.Fatalf("unexpected call %d", calls)
			return nil, nil
		}
	}}
	delivery, err := New(sinkConfig("type", "transient"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(ctx, "final"); errs.Classify(err) != errs.Cancelled {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestExternalDeliveryTimeoutsAreSinkFailures(t *testing.T) {
	const timeout = 20 * time.Millisecond
	tests := []struct {
		name          string
		mode          string
		clipboardMode string
		blockCall     int
	}{
		{name: "copy-final", mode: "clipboard", clipboardMode: "clean-only", blockCall: 1},
		{name: "paste", mode: "paste", clipboardMode: "clean-only", blockCall: 2},
		{name: "type", mode: "type", clipboardMode: "none", blockCall: 1},
		{name: "clear", mode: "type", clipboardMode: "transient", blockCall: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			executor := &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, _ string, _ []string, _ []byte) ([]byte, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Error("commande sans deadline")
				}
				if calls != test.blockCall {
					return nil, nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			delivery, err := New(sinkConfig(test.mode, test.clipboardMode), Options{Executor: executor})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := delivery.Deliver(ctx, "final"); errs.Classify(err) != errs.SinkFailed {
				t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
			}
		})
	}
}

func TestRawClipboardUsesDeliveryTimeout(t *testing.T) {
	const timeout = 20 * time.Millisecond
	active := atomic.Int32{}
	executor := &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, _ string, _ []string, _ []byte) ([]byte, error) {
		active.Add(1)
		defer active.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	delivery, err := New(sinkConfig("stdout", "raw-and-clean"), Options{Executor: executor, Stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	raw := delivery.(interface {
		CopyRaw(context.Context, string) error
	})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := raw.CopyRaw(ctx, "raw"); errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	if active.Load() != 0 {
		t.Fatalf("commandes encore actives = %d", active.Load())
	}
}
