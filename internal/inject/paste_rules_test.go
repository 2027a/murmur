package inject

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func windowReply(class string) string {
	data, _ := json.Marshal(map[string]string{"address": "0x123", "class": class, "title": "private window title"})
	return string(data)
}

func TestApplicationPasteRuleSelection(t *testing.T) {
	rules := []PasteRule{{Class: "kitty", Shortcut: "ctrl+shift+v"}, {Class: "special-editor", Shortcut: "shift+insert"}, {Class: "kitty-dropterm", Shortcut: "ctrl+shift+v"}, {Class: "kitty", Shortcut: "ctrl+v"}}
	for _, provider := range []string{"lua", "hyprlang"} {
		for _, test := range []struct{ class, shortcut string }{
			{"kitty", "ctrl+shift+v"}, {"KiTtY", "ctrl+shift+v"}, {"kitty-extra", "ctrl+v"}, {"kitty-dropterm", "ctrl+shift+v"},
			{"Geany", "ctrl+v"}, {"org.gnome.Zenity", "ctrl+v"}, {"org.telegram.desktop", "ctrl+v"},
			{"special-editor", "shift+insert"}, {"kitty.*", "ctrl+v"}, {`x\");os.execute('bad')`, "ctrl+v"},
		} {
			t.Run(provider+"/"+test.class, func(t *testing.T) {
				executor := hyprlandExecutor(`{"configProvider":"`+provider+`"}`, windowReply(test.class), windowReply(test.class), "ok")
				injector, err := NewWithBackend(executor, []string{"ctrl+v", "ctrl+shift+v"}, Space, Hyprland, rules...)
				if err != nil {
					t.Fatal(err)
				}
				if err := injector.Paste(context.Background(), "private transcript"); err != nil {
					t.Fatal(err)
				}
				calls := executor.Calls()
				if len(calls) != 4 {
					t.Fatalf("expected one dispatch: %#v", calls)
				}
				mods, key := hyprlandShortcut(test.shortcut)
				want := []string{"dispatch", "sendshortcut", mods + "," + key + ",address:0x123"}
				if provider == "lua" {
					want = []string{"dispatch", `hl.dsp.send_shortcut({mods="` + mods + `",key="` + key + `",window="address:0x123"})`}
				}
				if !reflect.DeepEqual(calls[3].Args, want) || len(calls[3].Stdin) != 0 {
					t.Fatalf("dispatch=%#v want=%v", calls[3], want)
				}
				for _, call := range calls {
					if strings.Contains(strings.Join(call.Args, " "), "private") || strings.Contains(strings.Join(call.Args, " "), "os.execute") {
						t.Fatal("window metadata or transcript entered executable arguments")
					}
				}
			})
		}
	}
}

func TestApplicationRulesRejectMissingOrChangedClass(t *testing.T) {
	for _, windows := range [][]string{
		{`{"address":"0x123"}`}, {windowReply("")}, {windowReply(" kitty")},
		{windowReply("kitty"), windowReply("Geany")},
		{windowReply("kitty"), `{"address":"0x123"}`},
	} {
		replies := append([]string{`{"configProvider":"lua"}`}, windows...)
		executor := hyprlandExecutor(replies...)
		injector, _ := NewWithBackend(executor, []string{"ctrl+v"}, Space, Hyprland, PasteRule{Class: "kitty", Shortcut: "ctrl+shift+v"})
		err := injector.Paste(context.Background(), "private transcript")
		if errs.Classify(err) != errs.SinkFailed || strings.Contains(err.Error(), "private") {
			t.Fatalf("error=%v", err)
		}
		for _, call := range executor.Calls() {
			if call.Name != "hyprctl" || call.Args[0] != "-j" {
				t.Fatalf("keys sent after failed classification: %#v", call)
			}
		}
	}
}

func TestApplicationRuleValidationAndCopy(t *testing.T) {
	for _, rule := range []PasteRule{
		{Class: "", Shortcut: "ctrl+v"}, {Class: "kitty\n", Shortcut: "ctrl+v"},
		{Class: "kitty", Shortcut: ""}, {Class: "kitty", Shortcut: "ctrl+v;bad"},
		{Class: strings.Repeat("x", 257), Shortcut: "ctrl+v"},
		{Class: string([]byte{0xff}), Shortcut: "ctrl+v"},
	} {
		executor := &sysexec.FakeExecutor{}
		if _, err := NewWithBackend(executor, []string{"ctrl+v"}, Space, Hyprland, rule); errs.Classify(err) != errs.ConfigInvalid {
			t.Fatalf("invalid rule accepted: %#v %v", rule, err)
		}
		if len(executor.Calls()) != 0 {
			t.Fatal("constructor ran a command")
		}
	}
	executor := &sysexec.FakeExecutor{}
	if _, err := NewWithBackend(executor, []string{"ctrl+v"}, Space, Wtype, PasteRule{Class: "kitty", Shortcut: "ctrl+shift+v"}); errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("wtype silently ignored rules: %v", err)
	}
	rules := []PasteRule{{Class: "kitty", Shortcut: "ctrl+shift+v"}}
	injector, err := NewWithBackend(executor, []string{"ctrl+v"}, Space, Hyprland, rules...)
	if err != nil {
		t.Fatal(err)
	}
	rules[0].Shortcut = "ctrl+v"
	if got := injector.shortcutForClass("kitty"); got != "ctrl+shift+v" {
		t.Fatalf("caller mutation changed rule: %q", got)
	}
	many := make([]PasteRule, 65)
	for i := range many {
		many[i] = PasteRule{Class: "kitty", Shortcut: "ctrl+v"}
	}
	if _, err := NewWithBackend(executor, []string{"ctrl+v"}, Space, Hyprland, many...); errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("too many rules accepted: %v", err)
	}
}

func TestMatchedRuleDispatchFailureNeverRetries(t *testing.T) {
	executor := hyprlandExecutor(`{"configProvider":"lua"}`, windowReply("kitty"), windowReply("kitty"), "failed")
	injector, _ := NewWithBackend(executor, []string{"ctrl+v", "shift+insert"}, Space, Hyprland, PasteRule{Class: "kitty", Shortcut: "ctrl+shift+v"})
	if err := injector.Paste(context.Background(), "unused"); errs.Classify(err) != errs.SinkFailed {
		t.Fatalf("error=%v", err)
	}
	if len(executor.Calls()) != 4 {
		t.Fatalf("retry after failed application-specific dispatch: %#v", executor.Calls())
	}
}
