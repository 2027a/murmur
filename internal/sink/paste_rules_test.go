package sink

import (
	"context"
	"reflect"
	"testing"

	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func TestApplicationRulesReachDelivery(t *testing.T) {
	for _, class := range []string{"kitty", "Geany"} {
		for _, mode := range []string{"paste", "auto"} {
			cfg := sinkConfig(mode, "clean-only")
			cfg.Sink.PasteBackend = "hyprland"
			cfg.Sink.PasteRules = []config.PasteRule{{Class: "kitty", Shortcut: "ctrl+shift+v"}}
			executor := &sysexec.FakeExecutor{RunFunc: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
				if name == "wl-copy" {
					return nil, nil
				}
				switch args[1] {
				case "status":
					return []byte(`{"configProvider":"lua"}`), nil
				case "activewindow":
					return []byte(`{"address":"0x123","class":"` + class + `"}`), nil
				default:
					return []byte("ok"), nil
				}
			}}
			target, err := New(cfg, Options{Executor: executor})
			if err != nil {
				t.Fatal(err)
			}
			if err := target.Deliver(context.Background(), "one\ntwo"); err != nil {
				t.Fatal(err)
			}
			calls := executor.Calls()
			mods := "CTRL"
			if class == "kitty" {
				mods = "CTRL SHIFT"
			}
			want := []string{"dispatch", `hl.dsp.send_shortcut({mods="` + mods + `",key="v",window="address:0x123"})`}
			if len(calls) != 5 || string(calls[0].Stdin) != "one two" || !reflect.DeepEqual(calls[4].Args, want) {
				t.Fatalf("class=%s mode=%s calls=%#v", class, mode, calls)
			}
		}
	}
}

func TestUnusedApplicationRulesDoNotProbeHyprland(t *testing.T) {
	for _, test := range []struct {
		mode, clipboard string
		only            bool
		emptyShortcuts  bool
		want            []string
	}{
		{"paste", "clean-only", true, false, []string{"wl-copy"}},
		{"type", "none", false, false, []string{"wtype"}},
		{"paste", "none", false, false, []string{"wtype"}},
		{"paste", "clean-only", false, true, []string{"wl-copy", "wtype"}},
	} {
		cfg := sinkConfig(test.mode, test.clipboard)
		cfg.Sink.PasteBackend = "hyprland"
		cfg.Sink.PasteRules = []config.PasteRule{{Class: "kitty", Shortcut: "ctrl+shift+v"}}
		if test.emptyShortcuts {
			cfg.Sink.PasteShortcuts = nil
		}
		executor := &sysexec.FakeExecutor{}
		target, err := New(cfg, Options{Executor: executor, ClipboardOnly: test.only})
		if err != nil {
			t.Fatal(err)
		}
		if err := target.Deliver(context.Background(), "safe"); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, call := range executor.Calls() {
			names = append(names, call.Name)
		}
		if !reflect.DeepEqual(names, test.want) {
			t.Fatalf("unexpected commands: %v", names)
		}
	}
}
