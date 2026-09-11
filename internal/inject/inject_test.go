package inject

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

func (f *fakeExecutor) RunDiscard(ctx context.Context, name string, args []string, stdin []byte) error {
	_, err := f.Run(ctx, name, args, stdin)
	return err
}

func TestPasteShortcutArguments(t *testing.T) {
	tests := []struct {
		shortcut string
		args     []string
	}{
		{"ctrl+shift+v", []string{"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"}},
		{"shift+insert", []string{"-M", "shift", "Insert", "-m", "shift"}},
		{"ctrl+v", []string{"-M", "ctrl", "v", "-m", "ctrl"}},
	}
	for _, test := range tests {
		t.Run(test.shortcut, func(t *testing.T) {
			executor := &fakeExecutor{}
			injector, err := New(executor, []string{test.shortcut}, Space)
			if err != nil {
				t.Fatal(err)
			}
			if err := injector.Paste(context.Background(), "fallback"); err != nil {
				t.Fatal(err)
			}
			want := []call{{name: "wtype", args: test.args}}
			if !reflect.DeepEqual(executor.calls, want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, want)
			}
		})
	}
}

func TestPasteUsesOnlyFirstShortcut(t *testing.T) {
	failure := errors.New("unsupported")
	executor := &fakeExecutor{results: []error{failure}}
	injector, err := New(executor, []string{"ctrl+shift+v", "shift+insert", "ctrl+v"}, Space)
	if err != nil {
		t.Fatal(err)
	}
	if err := injector.Paste(context.Background(), "unused"); !errors.Is(err, failure) || errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	if len(executor.calls) != 1 || !reflect.DeepEqual(executor.calls[0].args, []string{"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"}) {
		t.Fatalf("calls = %#v", executor.calls)
	}
}

func TestPasteWithoutShortcutTypesOnce(t *testing.T) {
	executor := &fakeExecutor{}
	injector, err := New(executor, nil, Space)
	if err != nil {
		t.Fatal(err)
	}
	if err := injector.Paste(context.Background(), "one\ntwo"); err != nil {
		t.Fatal(err)
	}
	want := call{name: "wtype", args: []string{"-"}, stdin: "one two"}
	if len(executor.calls) != 1 || !reflect.DeepEqual(executor.calls[0], want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, []call{want})
	}
}

func TestTypeNewlineModes(t *testing.T) {
	tests := []struct {
		mode NewlineMode
		want string
	}{
		{Space, "one two three"},
		{Marker, "one [NL] two [NL] three"},
		{DangerousKeep, "one\ntwo\nthree"},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			executor := &fakeExecutor{}
			injector, err := New(executor, nil, test.mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := injector.Type(context.Background(), "one\r\ntwo\nthree"); err != nil {
				t.Fatal(err)
			}
			want := []call{{name: "wtype", args: []string{"-"}, stdin: test.want}}
			if !reflect.DeepEqual(executor.calls, want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, want)
			}
		})
	}
}

func TestSanitizeTextRemovesControlsAndPreservesUnicode(t *testing.T) {
	input := "a\r\nb\nc\rd\te\x1bf\x00g\x7fh\u0085i\u009fjé🙂\u2028" + string([]byte{0xff})
	tests := []struct {
		mode NewlineMode
		want string
	}{
		{Space, "a b c d efghijé🙂\u2028�"},
		{Marker, "a [NL] b [NL] c [NL] d efghijé🙂\u2028�"},
		{DangerousKeep, "a\nb\nc\nd efghijé🙂\u2028�"},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			if got := SanitizeText(input, test.mode); got != test.want {
				t.Fatalf("SanitizeText() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPasteSingleShortcutHasDeadline(t *testing.T) {
	var calls int
	executor := &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, _ string, _ []string, _ []byte) ([]byte, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("commande sans deadline")
		}
		return nil, errors.New("échec factice")
	}}
	injector, err := New(executor, []string{"ctrl+shift+v", "shift+insert"}, Space)
	if err != nil {
		t.Fatal(err)
	}
	if err := injector.Paste(context.Background(), "fallback"); errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestBlockedInjectionProcessesAreKilledAndReaped(t *testing.T) {
	for _, test := range []string{"paste", "type"} {
		t.Run(test, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			writeBlockingTool(t, filepath.Join(dir, "wtype"))
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			executor := sysexec.OSExecutor{Env: []string{"PATH=" + dir, "MURMUR_FAKE_PID=" + pidFile}}
			injector, err := New(executor, []string{"ctrl+v"}, Space)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- runWithContext(ctx, injector, test) }()
			waitForFileWhileRunning(t, pidFile, result)
			select {
			case err := <-result:
				if errs.Classify(err) != errs.SinkFailed {
					t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
				}
			case <-time.After(2 * time.Second):
				t.Fatal("l'injection bloquée n'a pas terminé après sa deadline")
			}
			assertProcessGone(t, pidFile)
		})
	}
}

func waitForFileWhileRunning(t *testing.T, path string, result <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			t.Fatalf("l'injection a terminé avant le démarrage du processus factice: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	t.Fatalf("le processus factice n'a pas créé %s", path)
}

func runWithContext(ctx context.Context, injector *Injector, operation string) error {
	if operation == "paste" {
		return injector.Paste(ctx, "fallback")
	}
	return injector.Type(ctx, strings.Repeat("x", 1<<20))
}

func writeBlockingTool(t *testing.T, path string) {
	t.Helper()
	script := "#!/bin/sh\ntrap '' HUP INT TERM\n/bin/sh -c 'trap \"\" HUP INT TERM; while :; do /bin/sleep 30; done' &\nprintf '%s\\n%s\\n' \"$$\" \"$!\" > \"$MURMUR_FAKE_PID\"\nwait\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func assertProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			err = syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("processus %d encore présent: %v", pid, err)
		}
	}
}
