package inject

import (
	"context"
	"fmt"
	"strings"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

type NewlineMode string

const (
	Keep   NewlineMode = "keep"
	Space  NewlineMode = "space"
	Marker NewlineMode = "marker"
)

const newlineMarker = " [NL] "

type Injector struct {
	executor  sysexec.Executor
	shortcuts [][]string
	newlines  NewlineMode
}

func New(executor sysexec.Executor, shortcuts []string, newlines NewlineMode) (*Injector, error) {
	if executor == nil {
		return nil, errs.New(errs.ConfigInvalid, "inject", "aucun exécuteur disponible")
	}
	if newlines != Keep && newlines != Space && newlines != Marker {
		return nil, errs.New(errs.ConfigInvalid, "inject", fmt.Sprintf("mode de retours à la ligne invalide: %q", newlines))
	}
	parsed := make([][]string, len(shortcuts))
	for i, shortcut := range shortcuts {
		args, ok := shortcutArgs(shortcut)
		if !ok {
			return nil, errs.New(errs.ConfigInvalid, "inject", fmt.Sprintf("raccourci de collage inconnu: %q", shortcut))
		}
		parsed[i] = args
	}
	return &Injector{executor: executor, shortcuts: parsed, newlines: newlines}, nil
}

// Type sends text to wtype's stdin after applying the newline policy.
func (i *Injector) Type(ctx context.Context, text string) error {
	text = formatNewlines(text, i.newlines)
	if _, err := i.executor.Run(ctx, "wtype", []string{"-"}, []byte(text)); err != nil {
		return errs.Wrap(errs.SinkFailed, "inject", "frappe simulée impossible", err)
	}
	return nil
}

// Paste tries each configured shortcut and falls back to simulated typing only
// when every shortcut command failed technically.
func (i *Injector) Paste(ctx context.Context, text string) error {
	for _, args := range i.shortcuts {
		if _, err := i.executor.Run(ctx, "wtype", args, nil); err == nil {
			return nil
		}
	}
	return i.Type(ctx, text)
}

func shortcutArgs(shortcut string) ([]string, bool) {
	switch shortcut {
	case "ctrl+shift+v":
		return []string{"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"}, true
	case "shift+insert":
		return []string{"-M", "shift", "Insert", "-m", "shift"}, true
	case "ctrl+v":
		return []string{"-M", "ctrl", "v", "-m", "ctrl"}, true
	default:
		return nil, false
	}
}

func formatNewlines(text string, mode NewlineMode) string {
	if mode == Keep {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if mode == Space {
		return strings.ReplaceAll(text, "\n", " ")
	}
	return strings.ReplaceAll(text, "\n", newlineMarker)
}
