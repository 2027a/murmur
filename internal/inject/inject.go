package inject

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"git.2027a.net/2027a/murmur/internal/delivery"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

type NewlineMode string

const (
	Space         NewlineMode = "space"
	Marker        NewlineMode = "marker"
	DangerousKeep NewlineMode = "dangerous-keep"
)

const newlineMarker = " [NL] "

type Injector struct {
	executor  sysexec.Executor
	shortcuts [][]string
	newlines  NewlineMode
	backend   PasteBackend
	shortcut  string
	rules     []PasteRule
}

// New keeps the historical wtype backend for existing callers.
func New(executor sysexec.Executor, shortcuts []string, newlines NewlineMode) (*Injector, error) {
	return NewWithBackend(executor, shortcuts, newlines, Wtype)
}

func NewWithBackend(executor sysexec.Executor, shortcuts []string, newlines NewlineMode, backend PasteBackend, rules ...PasteRule) (*Injector, error) {
	if backend != Wtype && backend != Hyprland {
		return nil, errs.New(errs.ConfigInvalid, "inject", "backend de collage inconnu")
	}
	if len(rules) > 0 && backend != Hyprland {
		return nil, errs.New(errs.ConfigInvalid, "inject", "les règles par application exigent le backend hyprland")
	}
	if err := validatePasteRules(rules); err != nil {
		return nil, errs.Wrap(errs.ConfigInvalid, "inject", "règles de collage invalides", err)
	}
	if executor == nil {
		return nil, errs.New(errs.ConfigInvalid, "inject", "aucun exécuteur disponible")
	}
	if newlines != Space && newlines != Marker && newlines != DangerousKeep {
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
	var first string
	if len(shortcuts) > 0 {
		first = shortcuts[0]
	}
	return &Injector{executor: executor, shortcuts: parsed, newlines: newlines, backend: backend, shortcut: first, rules: append([]PasteRule(nil), rules...)}, nil
}

// Type sends text to wtype's stdin after applying the newline policy.
func (i *Injector) Type(ctx context.Context, text string) error {
	commandCtx, cancel := context.WithTimeout(ctx, delivery.CommandTimeout)
	defer cancel()
	return i.typeText(commandCtx, text)
}

func (i *Injector) typeText(ctx context.Context, text string) error {
	text = SanitizeText(text, i.newlines)
	if err := sysexec.RunDiscard(ctx, i.executor, "wtype", []string{"-"}, []byte(text)); err != nil {
		return commandError(ctx, "frappe simulée impossible", err)
	}
	return nil
}

// Paste performs a single active injection attempt: an application rule or the
// first configured shortcut, or direct typing when no shortcut is configured.
func (i *Injector) Paste(ctx context.Context, text string) error {
	commandCtx, cancel := context.WithTimeout(ctx, delivery.CommandTimeout)
	defer cancel()
	if len(i.shortcuts) == 0 {
		return i.typeText(commandCtx, text)
	}
	if i.backend == Hyprland {
		return i.pasteHyprland(commandCtx)
	}
	if err := sysexec.RunDiscard(commandCtx, i.executor, "wtype", i.shortcuts[0], nil); err != nil {
		return commandError(commandCtx, "collage simulé impossible", err)
	}
	return nil
}

func commandError(ctx context.Context, message string, cause error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errs.New(errs.SinkFailed, "inject", message+": délai de livraison dépassé")
	}
	return errs.Wrap(errs.SinkFailed, "inject", message, cause)
}

func shortcutArgs(shortcut string) ([]string, bool) {
	switch shortcut {
	case "ctrl+shift+v":
		return []string{"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"}, true
	case "shift+insert":
		return []string{"-M", "shift", "-k", "Insert", "-m", "shift"}, true
	case "ctrl+v":
		return []string{"-M", "ctrl", "v", "-m", "ctrl"}, true
	default:
		return nil, false
	}
}

// SanitizeText prepares untrusted text for active paste or typing. Tabs become
// spaces, invalid UTF-8 becomes U+FFFD, and Unicode control characters are
// removed except for explicitly opted-in line feeds.
func SanitizeText(text string, mode NewlineMode) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	switch mode {
	case Marker:
		text = strings.ReplaceAll(text, "\n", newlineMarker)
	case DangerousKeep:
	default:
		text = strings.ReplaceAll(text, "\n", " ")
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t':
			return ' '
		case '\n':
			return r
		default:
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}
	}, text)
}
