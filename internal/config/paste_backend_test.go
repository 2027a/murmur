package config

import (
	"strings"
	"testing"

	"git.2027a.net/2027a/murmur/internal/errs"
)

func TestPasteBackendResolutionAndPrecedence(t *testing.T) {
	if Defaults().Sink.PasteBackend != "wtype" {
		t.Fatal("historical backend changed")
	}
	for _, backend := range []string{"wtype", "hyprland", "", "auto", "HYPRLAND"} {
		cfg, err := ResolveWithOptions(ResolveOptions{
			Path:      writeConfig(t, "[sink]\npaste_backend = \""+backend+"\"\n"),
			LookupEnv: func(string) (string, bool) { return "", false },
		})
		if backend == "wtype" || backend == "hyprland" {
			if err != nil || cfg.Sink.PasteBackend != backend {
				t.Fatalf("backend %q: cfg=%#v error=%v", backend, cfg.Sink, err)
			}
		} else if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "sink.paste_backend") {
			t.Fatalf("invalid backend %q: %v", backend, err)
		}
	}
	path := writeConfig(t, "[sink]\npaste_backend = \"wtype\"\n")
	env := func(name string) (string, bool) { return "hyprland", name == "MURMUR_SINK_PASTE_BACKEND" }
	cfg, err := ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: env})
	if err != nil || cfg.Sink.PasteBackend != "hyprland" {
		t.Fatalf("environment backend=%q error=%v", cfg.Sink.PasteBackend, err)
	}
	backend := "wtype"
	cfg, err = ResolveWithOptions(ResolveOptions{Path: path, LookupEnv: env, Overrides: Overrides{PasteBackend: &backend}})
	if err != nil || cfg.Sink.PasteBackend != "wtype" {
		t.Fatalf("override backend=%q error=%v", cfg.Sink.PasteBackend, err)
	}
	cfg = Defaults()
	cfg.Sink.PasteBackend = "hyprland"
	cfg.Sink.ClipboardMode = "transient"
	if errs.Classify(Validate(cfg)) != errs.ConfigInvalid {
		t.Fatal("Hyprland must not allow transient clipboard with a paste shortcut")
	}
}
