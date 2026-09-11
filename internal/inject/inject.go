package inject

import (
	"context"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"git.2027a.net/mathieu/murmur/internal/delivery"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
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
}

func New(executor sysexec.Executor, shortcuts []string, newlines NewlineMode) (*Injector, error) {
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
	return &Injector{executor: executor, shortcuts: parsed, newlines: newlines}, nil
}

// Type sends text to wtype's stdin after applying the newline policy.
func (i *Injector) Type(ctx context.Context, text string) error {
	text = SanitizeText(text, i.newlines)
	return i.TypeSanitized(ctx, text)
}

// TypeSanitized sends already-sanitized text without transforming it again.
func (i *Injector) TypeSanitized(ctx context.Context, text string) error {
	commandCtx, cancel := context.WithTimeout(ctx, delivery.CommandTimeout)
	defer cancel()
	return i.typeText(commandCtx, text)
}

func (i *Injector) typeText(ctx context.Context, text string) error {
	if err := sysexec.RunDiscard(ctx, i.executor, "wtype", []string{"-"}, []byte(text)); err != nil {
		return commandError(ctx, "frappe simulée impossible", err)
	}
	return nil
}

// Paste performs a single active injection attempt: the first configured
// shortcut, or direct typing when no shortcut is configured.
func (i *Injector) Paste(ctx context.Context, text string) error {
	return i.PasteSanitized(ctx, SanitizeText(text, i.newlines))
}

// PasteSanitized injects text that already passed the active-text policy.
func (i *Injector) PasteSanitized(ctx context.Context, text string) error {
	commandCtx, cancel := context.WithTimeout(ctx, delivery.CommandTimeout)
	defer cancel()
	if len(i.shortcuts) == 0 {
		return i.typeText(commandCtx, text)
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
		return []string{"-M", "shift", "Insert", "-m", "shift"}, true
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
	result, _ := SanitizeTextBounded(text, mode, 0)
	return result
}

// SanitizeTextBounded applies the active-text policy in one pass and refuses
// output larger than maxBytes. A non-positive maxBytes disables the bound.
func SanitizeTextBounded(text string, mode NewlineMode, maxBytes int) (string, error) {
	if mode != Space && mode != Marker && mode != DangerousKeep {
		return "", errs.New(errs.ConfigInvalid, "inject", "mode de retours à la ligne invalide")
	}
	capacity := len(text)
	if maxBytes > 0 && capacity > maxBytes {
		capacity = maxBytes
	}
	output := make([]byte, 0, capacity)
	appendValue := func(value string) error {
		if maxBytes > 0 && len(output)+len(value) > maxBytes {
			return errs.New(errs.ServerError, "inject", "texte sanitisé supérieur au budget configuré")
		}
		output = append(output, value...)
		return nil
	}
	for offset := 0; offset < len(text); {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == utf8.RuneError && size == 1 {
			r = utf8.RuneError
		}
		offset += size
		if r == '\r' {
			if offset < len(text) && text[offset] == '\n' {
				offset++
			}
			r = '\n'
		}
		switch r {
		case '	':
			if err := appendValue(" "); err != nil {
				return "", err
			}
		case '\n':
			value := " "
			if mode == Marker {
				value = newlineMarker
			} else if mode == DangerousKeep {
				value = "\n"
			}
			if err := appendValue(value); err != nil {
				return "", err
			}
		default:
			if unicode.IsControl(r) {
				continue
			}
			if err := appendValue(string(r)); err != nil {
				return "", err
			}
		}
	}
	return string(output), nil
}
