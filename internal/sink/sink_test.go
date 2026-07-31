package sink

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
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
		{name: "wtype", args: []string{"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"}},
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
	want := []call{{name: "wtype", args: []string{"-"}, stdin: "one\ntwo"}}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, want)
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

	t.Run("transient", func(t *testing.T) {
		executor := &fakeExecutor{results: []error{nil, failure, failure, failure, failure, nil}}
		delivery, err := New(sinkConfig("paste", "transient"), Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), "final"); errs.Classify(err) != errs.SinkFailed {
			t.Fatalf("error = %v", err)
		}
		last := executor.calls[len(executor.calls)-1]
		if last.name != "wl-copy" || !reflect.DeepEqual(last.args, []string{"--clear"}) {
			t.Fatalf("last call = %#v", last)
		}
	})
}
