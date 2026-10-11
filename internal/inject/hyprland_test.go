package inject

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func hyprlandExecutor(replies ...string) *sysexec.FakeExecutor {
	index := 0
	return &sysexec.FakeExecutor{RunFunc: func(_ context.Context, _ string, _ []string, _ []byte) ([]byte, error) {
		if index >= len(replies) {
			return nil, errors.New("unexpected command")
		}
		output := replies[index]
		index++
		return []byte(output), nil
	}}
}

func TestHyprlandPasteShortcuts(t *testing.T) {
	for _, provider := range []string{"lua", "hyprlang"} {
		for _, shortcut := range []string{"ctrl+shift+v", "shift+insert", "ctrl+v"} {
			t.Run(provider+"/"+shortcut, func(t *testing.T) {
				executor := hyprlandExecutor(`{"configProvider":"`+provider+`"}`, `{"address":"0x123abc","title":"private"}`, `{"address":"0x123abc"}`, "ok\n")
				injector, err := NewWithBackend(executor, []string{shortcut, "ctrl+v"}, Space, Hyprland)
				if err != nil {
					t.Fatal(err)
				}
				if err := injector.Paste(context.Background(), "untrusted text\"); os.execute('bad')"); err != nil {
					t.Fatal(err)
				}
				mods, key := hyprlandShortcut(shortcut)
				args := []string{"dispatch", "sendshortcut", mods + "," + key + ",address:0x123abc"}
				if provider == "lua" {
					args = []string{"dispatch", `hl.dsp.send_shortcut({mods="` + mods + `",key="` + key + `",window="address:0x123abc"})`}
				}
				want := []sysexec.Call{
					{Name: "hyprctl", Args: []string{"-j", "status"}},
					{Name: "hyprctl", Args: []string{"-j", "activewindow"}},
					{Name: "hyprctl", Args: []string{"-j", "activewindow"}},
					{Name: "hyprctl", Args: args},
				}
				if !reflect.DeepEqual(executor.Calls(), want) {
					t.Fatalf("calls = %#v, want %#v", executor.Calls(), want)
				}
			})
		}
	}
}

func TestHyprlandRejectsInvalidPreparationWithoutSendingKeys(t *testing.T) {
	for _, test := range []struct {
		name    string
		replies []string
	}{
		{"invalid status JSON", []string{"private malformed status"}},
		{"missing provider", []string{`{}`}},
		{"unknown provider", []string{`{"configProvider":"future"}`}},
		{"empty window", []string{`{"configProvider":"lua"}`, `{}`}},
		{"invalid window JSON", []string{`{"configProvider":"lua"}`, `private malformed window`}},
		{"focus changed", []string{`{"configProvider":"lua"}`, `{"address":"0x123"}`, `{"address":"0x456"}`}},
		{"window disappeared", []string{`{"configProvider":"lua"}`, `{"address":"0x123"}`, `{}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := hyprlandExecutor(test.replies...)
			injector, err := NewWithBackend(executor, []string{"ctrl+shift+v"}, Space, Hyprland)
			if err != nil {
				t.Fatal(err)
			}
			err = injector.Paste(context.Background(), "unused")
			if errs.Classify(err) != errs.SinkFailed || strings.Contains(err.Error(), "private") {
				t.Fatalf("error = %v", err)
			}
			for _, call := range executor.Calls() {
				if call.Name != "hyprctl" || call.Args[0] != "-j" {
					t.Fatalf("active action during failed preparation: %#v", call)
				}
			}
		})
	}
	for _, address := range []string{"", "0x0", "0x0000", "0x", "0x+1", "0x123,.*", "0x123\");os.execute('bad')", " 0x123", "0x123 ", "0x10000000000000000", "123"} {
		t.Run(address, func(t *testing.T) {
			if validWindowAddress(address) {
				t.Fatalf("unsafe address accepted: %q", address)
			}
		})
	}
}

func TestHyprlandCommandFailureNeverRetries(t *testing.T) {
	failure := errors.New("helper failed")
	for step := 1; step <= 4; step++ {
		executor := hyprlandExecutor(`{"configProvider":"lua"}`, `{"address":"0x123"}`, `{"address":"0x123"}`, "ok")
		run := executor.RunFunc
		calls := 0
		executor.RunFunc = func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
			calls++
			if calls == step {
				return nil, failure
			}
			return run(ctx, name, args, stdin)
		}
		injector, err := NewWithBackend(executor, []string{"ctrl+shift+v", "ctrl+v"}, Space, Hyprland)
		if err != nil {
			t.Fatal(err)
		}
		if err := injector.Paste(context.Background(), "unused"); !errors.Is(err, failure) || errs.Classify(err) != errs.SinkFailed {
			t.Fatalf("step %d error = %v", step, err)
		}
		if len(executor.Calls()) != step {
			t.Fatalf("step %d calls = %#v", step, executor.Calls())
		}
	}
	for _, reply := range []string{"", "error: private failure", "ok\nerror", "Invalid dispatcher", strings.Repeat("x", maxHyprlandOutput+1)} {
		executor := hyprlandExecutor(`{"configProvider":"lua"}`, `{"address":"0x123"}`, `{"address":"0x123"}`, reply)
		injector, _ := NewWithBackend(executor, []string{"ctrl+shift+v"}, Space, Hyprland)
		if err := injector.Paste(context.Background(), "unused"); errs.Classify(err) != errs.SinkFailed || strings.Contains(err.Error(), "private") {
			t.Fatalf("unexpected dispatcher reply accepted or leaked: %v", err)
		}
		if len(executor.Calls()) != 4 {
			t.Fatalf("retry after dispatch: %#v", executor.Calls())
		}
	}
}

func TestHyprlandUsesOneSharedDeadlineAndHonorsCancellation(t *testing.T) {
	executor := hyprlandExecutor(`{"configProvider":"lua"}`, `{"address":"0x123"}`, `{"address":"0x123"}`, "ok")
	run := executor.RunFunc
	var deadline time.Time
	executor.RunFunc = func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("command has no deadline")
		}
		if deadline.IsZero() {
			deadline = got
		} else if !deadline.Equal(got) {
			t.Fatal("budget was reset between queries and dispatch")
		}
		return run(ctx, name, args, stdin)
	}
	injector, _ := NewWithBackend(executor, []string{"ctrl+v"}, Space, Hyprland)
	if err := injector.Paste(context.Background(), "unused"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := injector.Paste(ctx, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled paste error = %v", err)
	}
	if len(executor.Calls()) != 4 {
		t.Fatal("cancelled paste ran a command")
	}
}

func TestHyprlandTypePathsStillUseWtype(t *testing.T) {
	for _, direct := range []bool{false, true} {
		executor := &sysexec.FakeExecutor{}
		shortcuts := []string{"ctrl+shift+v"}
		if !direct {
			shortcuts = nil
		}
		injector, err := NewWithBackend(executor, shortcuts, Space, Hyprland)
		if err != nil {
			t.Fatal(err)
		}
		if direct {
			err = injector.Type(context.Background(), "one\ntwo")
		} else {
			err = injector.Paste(context.Background(), "one\ntwo")
		}
		if err != nil {
			t.Fatal(err)
		}
		want := []sysexec.Call{{Name: "wtype", Args: []string{"-"}, Stdin: []byte("one two")}}
		if !reflect.DeepEqual(executor.Calls(), want) {
			t.Fatalf("calls = %#v", executor.Calls())
		}
	}
}

func TestHyprlandConstructorValidation(t *testing.T) {
	for _, test := range []struct {
		backend  PasteBackend
		shortcut string
	}{{"auto", "ctrl+v"}, {Hyprland, "ctrl+v;bad"}} {
		executor := &sysexec.FakeExecutor{}
		if _, err := NewWithBackend(executor, []string{test.shortcut}, Space, test.backend); errs.Classify(err) != errs.ConfigInvalid {
			t.Fatalf("error = %v", err)
		}
		if len(executor.Calls()) != 0 {
			t.Fatal("invalid constructor ran a command")
		}
	}
}

func TestBlockedHyprlandProcessesAreKilledAndReaped(t *testing.T) {
	for _, step := range []string{"status", "dispatch"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			writeBlockingTool(t, filepath.Join(dir, "block"))
			script := "#!/bin/sh\nexec \"" + filepath.Join(dir, "block") + "\"\n"
			if step == "dispatch" {
				script = "#!/bin/sh\ncase \"$2\" in\nstatus) printf '%s\\n' '{\"configProvider\":\"lua\"}' ;;\nactivewindow) printf '%s\\n' '{\"address\":\"0x123\"}' ;;\n*) exec \"" + filepath.Join(dir, "block") + "\" ;;\nesac\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			executor := sysexec.OSExecutor{Env: []string{"PATH=" + dir, "MURMUR_FAKE_PID=" + pidFile}}
			injector, _ := NewWithBackend(executor, []string{"ctrl+v"}, Space, Hyprland)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- injector.Paste(ctx, "unused") }()
			waitForFileWhileRunning(t, pidFile, result)
			select {
			case err := <-result:
				if errs.Classify(err) != errs.SinkFailed {
					t.Fatalf("error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("blocked helper did not stop")
			}
			assertProcessGone(t, pidFile)
		})
	}
}
