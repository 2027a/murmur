package config

import (
	"reflect"
	"strings"
	"testing"

	"git.2027a.net/2027a/murmur/internal/errs"
)

func TestPasteRulesDefaultAndTOML(t *testing.T) {
	cfg := Defaults()
	if !reflect.DeepEqual(cfg.Sink.PasteShortcuts, []string{"ctrl+v"}) || len(cfg.Sink.PasteRules) != 0 {
		t.Fatalf("defaults=%#v", cfg.Sink)
	}
	cfg, err := ResolveWithOptions(ResolveOptions{
		Path: writeConfig(t, `[sink]
paste_backend = "hyprland"
[[sink.paste_rules]]
class = "kitty"
shortcut = "ctrl+shift+v"
[[sink.paste_rules]]
class = "special-editor"
shortcut = "shift+insert"
`),
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	want := []PasteRule{{Class: "kitty", Shortcut: "ctrl+shift+v"}, {Class: "special-editor", Shortcut: "shift+insert"}}
	if err != nil || !reflect.DeepEqual(cfg.Sink.PasteRules, want) {
		t.Fatalf("rules=%#v error=%v", cfg.Sink.PasteRules, err)
	}
	// Existing explicit shortcut lists retain their historical first-element semantics.
	cfg, err = ResolveWithOptions(ResolveOptions{
		Path: writeConfig(t, `[sink]
paste_shortcuts = ["ctrl+shift+v", "ctrl+v"]
`), LookupEnv: func(string) (string, bool) { return "", false },
	})
	if err != nil || !reflect.DeepEqual(cfg.Sink.PasteShortcuts, []string{"ctrl+shift+v", "ctrl+v"}) {
		t.Fatalf("explicit list changed: %#v %v", cfg.Sink, err)
	}
}

func TestPasteRulesPreflightValidation(t *testing.T) {
	for _, test := range []struct {
		name, backend, class, shortcut string
	}{
		{"missing class", "hyprland", "", "ctrl+v"},
		{"space-only class", "hyprland", " ", "ctrl+v"},
		{"leading space", "hyprland", " kitty", "ctrl+v"},
		{"trailing space", "hyprland", "kitty ", "ctrl+v"},
		{"control class", "hyprland", "kitty\x1b", "ctrl+v"},
		{"invalid unicode", "hyprland", string([]byte{0xff}), "ctrl+v"},
		{"long class", "hyprland", strings.Repeat("x", 257), "ctrl+v"},
		{"missing shortcut", "hyprland", "kitty", ""},
		{"unknown shortcut", "hyprland", "kitty", "ctrl+v;bad"},
		{"wrong backend", "wtype", "kitty", "ctrl+shift+v"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Sink.PasteBackend = test.backend
			cfg.Sink.PasteRules = []PasteRule{{Class: test.class, Shortcut: test.shortcut}}
			if err := Validate(cfg); errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "sink.paste_rules") {
				t.Fatalf("invalid rules accepted: %v", err)
			}
		})
	}
	cfg := Defaults()
	cfg.Sink.PasteBackend = "hyprland"
	for i := 0; i < 64; i++ {
		cfg.Sink.PasteRules = append(cfg.Sink.PasteRules, PasteRule{Class: "kitty", Shortcut: "ctrl+v"})
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Sink.PasteRules = append(cfg.Sink.PasteRules, PasteRule{Class: "kitty", Shortcut: "ctrl+v"})
	if err := Validate(cfg); errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("too many rules accepted: %v", err)
	}
	_, err := ResolveWithOptions(ResolveOptions{Path: writeConfig(t, `[sink]
paste_backend = "hyprland"
[[sink.paste_rules]]
class = "kitty"
shortcut = "ctrl+shift+v"
unexpected = true
`), LookupEnv: func(string) (string, bool) { return "", false }})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("unknown rule key accepted: %v", err)
	}
}
