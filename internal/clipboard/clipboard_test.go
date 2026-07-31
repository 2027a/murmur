package clipboard

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
	mu         sync.Mutex
	calls      []call
	copyErrors []error
	clearErr   error
	selection  string
}

func (f *fakeExecutor) Run(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	if reflect.DeepEqual(args, []string{"--clear"}) {
		if f.clearErr == nil {
			f.selection = ""
		}
		return nil, f.clearErr
	}
	if len(f.copyErrors) > 0 {
		err := f.copyErrors[0]
		f.copyErrors = f.copyErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	f.selection = string(stdin)
	return nil, nil
}

func TestModesRunExactCommands(t *testing.T) {
	tests := []struct {
		name string
		mode Mode
		want []call
	}{
		{name: "raw-and-clean", mode: RawAndClean, want: []call{
			{name: "wl-copy", stdin: "raw"},
			{name: "wl-copy", stdin: "clean"},
		}},
		{name: "clean-only", mode: CleanOnly, want: []call{{name: "wl-copy", stdin: "clean"}}},
		{name: "none", mode: None},
		{name: "transient", mode: Transient, want: []call{
			{name: "wl-copy", stdin: "clean"},
			{name: "wl-copy", args: []string{"--clear"}},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeExecutor{}
			clipboard, err := New(executor, test.mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := clipboard.CopyRaw(context.Background(), "raw"); err != nil {
				t.Fatal(err)
			}
			if err := clipboard.CopyFinal(context.Background(), "clean"); err != nil {
				t.Fatal(err)
			}
			if err := clipboard.Clear(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(executor.calls, test.want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, test.want)
			}
		})
	}
}

func TestTransientClearFailureIsReported(t *testing.T) {
	executor := &fakeExecutor{clearErr: errors.New("selection owner gone")}
	clipboard, err := New(executor, Transient)
	if err != nil {
		t.Fatal(err)
	}
	if err := clipboard.Clear(context.Background()); err == nil {
		t.Fatal("Clear() error = nil")
	}
	if len(executor.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(executor.calls))
	}
}

func TestPersistentFinalCopyFailureLeavesRawWithoutClearAttempt(t *testing.T) {
	executor := &fakeExecutor{copyErrors: []error{nil, errors.New("copy final failed")}}
	clipboard, err := New(executor, RawAndClean)
	if err != nil {
		t.Fatal(err)
	}
	if err := clipboard.CopyRaw(context.Background(), "raw"); err != nil {
		t.Fatal(err)
	}
	if err := clipboard.CopyFinal(context.Background(), "final"); errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("CopyFinal() error = %v, class = %s", err, errs.Classify(err))
	}
	if err := clipboard.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executor.selection != "raw" {
		t.Fatalf("persistent selection = %q, want raw", executor.selection)
	}
	want := []call{{name: "wl-copy", stdin: "raw"}, {name: "wl-copy", stdin: "final"}}
	if !reflect.DeepEqual(executor.calls, want) {
		t.Fatalf("calls = %#v, want %#v", executor.calls, want)
	}
}

func TestNoneDoesNotRequireExecutor(t *testing.T) {
	clipboard, err := New(nil, None)
	if err != nil {
		t.Fatal(err)
	}
	if err := clipboard.CopyFinal(context.Background(), "ignored"); err != nil {
		t.Fatal(err)
	}
}

func TestEveryClipboardCommandHasDeadline(t *testing.T) {
	blocking := &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, _ string, _ []string, _ []byte) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("commande sans deadline")
		}
		if remaining := time.Until(deadline); remaining < 4*time.Second || remaining > 5*time.Second {
			t.Errorf("deadline dans %s", remaining)
		}
		return nil, errors.New("échec factice")
	}}
	tests := []struct {
		name string
		mode Mode
		run  func(*Clipboard) error
	}{
		{name: "raw", mode: RawAndClean, run: func(c *Clipboard) error { return c.CopyRaw(context.Background(), "raw") }},
		{name: "final", mode: CleanOnly, run: func(c *Clipboard) error { return c.CopyFinal(context.Background(), "final") }},
		{name: "clear", mode: Transient, run: func(c *Clipboard) error { return c.Clear(context.Background()) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			board, err := New(blocking, test.mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.run(board); errs.Classify(err) != errs.SinkFailed {
				t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
			}
		})
	}
}

func TestBlockedClipboardProcessIsKilledAndReaped(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	writeBlockingTool(t, filepath.Join(dir, "wl-copy"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	executor := sysexec.OSExecutor{Env: []string{"PATH=" + dir, "MURMUR_FAKE_PID=" + pidFile}}
	board, err := New(executor, CleanOnly)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := board.CopyFinal(ctx, strings.Repeat("x", 1<<20)); errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("error = %v, class = %s", err, errs.Classify(err))
	}
	assertProcessGone(t, pidFile)
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
