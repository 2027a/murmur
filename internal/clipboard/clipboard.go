package clipboard

import (
	"context"
	"errors"
	"fmt"

	"git.2027a.net/mathieu/murmur/internal/delivery"
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

// Clear removes a transient clipboard value and reports a failed attempt.
func (c *Clipboard) Clear(ctx context.Context) error {
	if c.mode != Transient {
		return nil
	}
	commandCtx, cancel := context.WithTimeout(ctx, delivery.CommandTimeout)
	defer cancel()
	if _, err := c.executor.Run(commandCtx, "wl-copy", []string{"--clear"}, nil); err != nil {
		return commandError(commandCtx, "effacement du presse-papiers impossible", err)
	}
	return nil
}

func (c *Clipboard) copy(ctx context.Context, text string) error {
	commandCtx, cancel := context.WithTimeout(ctx, delivery.CommandTimeout)
	defer cancel()
	if _, err := c.executor.Run(commandCtx, "wl-copy", nil, []byte(text)); err != nil {
		return commandError(commandCtx, "écriture du presse-papiers impossible", err)
	}
	return nil
}

func commandError(ctx context.Context, message string, cause error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errs.New(errs.SinkFailed, "clipboard", message+": délai de livraison dépassé")
	}
	return errs.Wrap(errs.SinkFailed, "clipboard", message, cause)
}
