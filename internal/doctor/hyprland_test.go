package doctor

import (
	"context"
	"reflect"
	"testing"

	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func TestHyprlandDoctorIsConditionalAndNeverSendsKeys(t *testing.T) {
	for _, test := range []struct {
		name, mode, auto, clipboard string
		shortcuts                   []string
		tools                       []string
		probe                       bool
	}{
		{"paste", "paste", "paste", "clean-only", []string{"ctrl+shift+v"}, []string{"hyprctl", "wl-copy"}, true},
		{"auto paste", "auto", "paste", "clean-only", []string{"ctrl+v"}, []string{"hyprctl", "wl-copy"}, true},
		{"type", "type", "paste", "clean-only", []string{"ctrl+v"}, []string{"wl-copy", "wtype"}, false},
		{"auto type", "auto", "type", "none", []string{"ctrl+v"}, []string{"wtype"}, false},
		{"no clipboard", "paste", "paste", "none", []string{"ctrl+v"}, []string{"wtype"}, false},
		{"no shortcuts", "paste", "paste", "clean-only", nil, []string{"wl-copy", "wtype"}, false},
		{"clipboard only", "clipboard", "paste", "clean-only", []string{"ctrl+v"}, []string{"wl-copy"}, false},
		{"stdout", "stdout", "paste", "none", []string{"ctrl+v"}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Sink.Mode, cfg.Sink.AutoMode, cfg.Sink.ClipboardMode = test.mode, test.auto, test.clipboard
			cfg.Sink.PasteBackend, cfg.Sink.PasteShortcuts = "hyprland", test.shortcuts
			cfg.Sink.PasteRules = []config.PasteRule{{Class: "kitty", Shortcut: "ctrl+shift+v"}}
			cfg.UI.Mode = "none"
			var tools []string
			executor := &sysexec.FakeExecutor{RunFunc: func(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("probe without deadline")
				}
				return []byte(`{"configProvider":"lua"}`), nil
			}}
			deps := baseDependencies(&fakeProviders{}, &rejectingSecrets{})
			deps.Executor = executor
			deps.LookPath = func(tool string) (string, error) {
				if tool != "pw-record" {
					tools = append(tools, tool)
				}
				return "/bin/" + tool, nil
			}
			report := Run(context.Background(), cfg, deps)
			if !report.OK || !reflect.DeepEqual(tools, test.tools) {
				t.Fatalf("report=%#v tools=%v want=%v", report, tools, test.tools)
			}
			calls := executor.Calls()
			if test.probe {
				want := []sysexec.Call{{Name: "hyprctl", Args: []string{"-j", "status"}}}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("probe calls=%#v", calls)
				}
			} else if len(calls) != 0 {
				t.Fatalf("unneeded session calls=%#v", calls)
			}
		})
	}
}

func TestHyprlandDoctorReportsUnavailableSession(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sink.PasteBackend = "hyprland"
	cfg.UI.Mode = "none"
	deps := baseDependencies(&fakeProviders{}, &rejectingSecrets{})
	for _, output := range []string{`{}`, `{"configProvider":"unsupported"}`, "unknown request"} {
		executor := &sysexec.FakeExecutor{RunFunc: func(context.Context, string, []string, []byte) ([]byte, error) {
			return []byte(output), nil
		}}
		deps.Executor = executor
		report := Run(context.Background(), cfg, deps)
		if report.OK {
			t.Fatalf("unsupported session accepted: %#v", report)
		}
		if calls := executor.Calls(); len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{"-j", "status"}) {
			t.Fatalf("unexpected probe calls=%#v", calls)
		}
	}
}
