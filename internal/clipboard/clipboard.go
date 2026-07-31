package clipboard

import (
	"context"
	"fmt"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

type Mode string

const (
	RawAndClean Mode = "raw-and-clean"
	CleanOnly   Mode = "clean-only"
	None        Mode = "none"
	Transient   Mode = "transient"
)

type Clipboard struct {
	executor sysexec.Executor
	mode     Mode
}

func New(executor sysexec.Executor, mode Mode) (*Clipboard, error) {
	if mode != RawAndClean && mode != CleanOnly && mode != None && mode != Transient {
		return nil, errs.New(errs.ConfigInvalid, "clipboard", fmt.Sprintf("mode invalide: %q", mode))
	}
	if executor == nil && mode != None {
		return nil, errs.New(errs.ConfigInvalid, "clipboard", "aucun exécuteur disponible")
	}
	return &Clipboard{executor: executor, mode: mode}, nil
}

func (c *Clipboard) Mode() Mode { return c.mode }

// CopyRaw only exposes the unprocessed transcript under raw-and-clean.
func (c *Clipboard) CopyRaw(ctx context.Context, text string) error {
	if c.mode != RawAndClean {
		return nil
	}
	return c.copy(ctx, text)
}

// CopyFinal applies the configured policy to the final text.
func (c *Clipboard) CopyFinal(ctx context.Context, text string) error {
	if c.mode == None {
		return nil
	}
	return c.copy(ctx, text)
}

// Clear removes a transient clipboard value on a best-effort basis.
func (c *Clipboard) Clear(ctx context.Context) {
	if c.mode != Transient {
		return
	}
	_, _ = c.executor.Run(ctx, "wl-copy", []string{"--clear"}, nil)
}

func (c *Clipboard) copy(ctx context.Context, text string) error {
	if _, err := c.executor.Run(ctx, "wl-copy", nil, []byte(text)); err != nil {
		return errs.Wrap(errs.SinkFailed, "clipboard", "écriture du presse-papiers impossible", err)
	}
	return nil
}
