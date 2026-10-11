package sink

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func TestHyprlandSinkDelivery(t *testing.T) {
	for _, mode := range []string{"paste", "auto"} {
		cfg := sinkConfig(mode, "clean-only")
		cfg.Sink.PasteBackend = "hyprland"
		executor := &sysexec.FakeExecutor{RunFunc: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
			if name == "wl-copy" {
				return nil, nil
			}
			switch args[1] {
			case "status":
				return []byte(`{"configProvider":"lua"}`), nil
			case "activewindow":
				return []byte(`{"address":"0x123"}`), nil
			default:
				return []byte("ok"), nil
			}
		}}
		delivery, err := New(cfg, Options{Executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), "one\ntwo\x1b"); err != nil {
			t.Fatal(err)
		}
		want := []sysexec.Call{
			{Name: "wl-copy", Stdin: []byte("one two")},
			{Name: "hyprctl", Args: []string{"-j", "status"}},
			{Name: "hyprctl", Args: []string{"-j", "activewindow"}},
			{Name: "hyprctl", Args: []string{"-j", "activewindow"}},
			{Name: "hyprctl", Args: []string{"dispatch", `hl.dsp.send_shortcut({mods="CTRL",key="v",window="address:0x123"})`}},
		}
		if !reflect.DeepEqual(executor.Calls(), want) {
			t.Fatalf("mode=%s calls=%#v", mode, executor.Calls())
		}
	}
}

func TestHyprlandSinkBypassesUnusedBackend(t *testing.T) {
	for _, test := range []struct {
		mode, clipboard string
		only            bool
		shortcuts       []string
		want            []sysexec.Call
	}{
		{"paste", "clean-only", true, []string{"ctrl+v"}, []sysexec.Call{{Name: "wl-copy", Stdin: []byte("one\ntwo")}}},
		{"paste", "none", false, []string{"ctrl+v"}, []sysexec.Call{{Name: "wtype", Args: []string{"-"}, Stdin: []byte("one two")}}},
		{"type", "none", false, []string{"ctrl+v"}, []sysexec.Call{{Name: "wtype", Args: []string{"-"}, Stdin: []byte("one two")}}},
		{"paste", "clean-only", false, nil, []sysexec.Call{{Name: "wl-copy", Stdin: []byte("one two")}, {Name: "wtype", Args: []string{"-"}, Stdin: []byte("one two")}}},
	} {
		cfg := config.Defaults()
		cfg.Sink.Mode, cfg.Sink.ClipboardMode = test.mode, test.clipboard
		cfg.Sink.PasteBackend, cfg.Sink.PasteShortcuts = "hyprland", test.shortcuts
		executor := &sysexec.FakeExecutor{}
		delivery, err := New(cfg, Options{Executor: executor, ClipboardOnly: test.only})
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.Deliver(context.Background(), "one\ntwo"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(executor.Calls(), test.want) {
			t.Fatalf("calls=%#v want=%#v", executor.Calls(), test.want)
		}
	}
}

func TestHyprlandSinkStopsAfterClipboardFailure(t *testing.T) {
	cfg := sinkConfig("paste", "clean-only")
	cfg.Sink.PasteBackend = "hyprland"
	failure := errors.New("clipboard failed")
	executor := &sysexec.FakeExecutor{RunFunc: func(context.Context, string, []string, []byte) ([]byte, error) { return nil, failure }}
	delivery, err := New(cfg, Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.Deliver(context.Background(), "final"); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if calls := executor.Calls(); len(calls) != 1 || calls[0].Name != "wl-copy" {
		t.Fatalf("calls=%#v", calls)
	}
}
