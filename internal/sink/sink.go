package sink

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"git.2027a.net/mathieu/murmur/internal/clipboard"
	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/inject"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
	"git.2027a.net/mathieu/murmur/internal/xdg"
)

type Mode string

const (
	Paste     Mode = "paste"
	Type      Mode = "type"
	Clipboard Mode = "clipboard"
	Stdout    Mode = "stdout"
	File      Mode = "file"
	Auto      Mode = "auto"
)

type Sink interface {
	Deliver(ctx context.Context, text string) error
}

type Options struct {
	Executor      sysexec.Executor
	Stdout        io.Writer
	ClipboardOnly bool
}

// New constructs the configured delivery chain. Clipboard-only is resolved
// before the injection package is constructed.
func New(cfg config.Config, options Options) (Sink, error) {
	if err := config.ValidateClipboardOnly(cfg, options.ClipboardOnly); err != nil {
		return nil, err
	}

	mode := Mode(cfg.Sink.Mode)
	if options.ClipboardOnly {
		mode = Clipboard
	} else if mode == Auto {
		mode = Mode(cfg.Sink.AutoMode)
	}

	clipboardMode := clipboard.Mode(cfg.Sink.ClipboardMode)
	board, err := clipboard.New(options.Executor, clipboardMode)
	if err != nil {
		return nil, err
	}
	if mode == Clipboard && (clipboardMode == clipboard.None || clipboardMode == clipboard.Transient) {
		return nil, errs.New(errs.ConfigInvalid, "sink", "le sink clipboard exige un mode de presse-papiers persistant")
	}

	var target Sink
	switch mode {
	case Paste, Type:
		injector, err := inject.New(options.Executor, cfg.Sink.PasteShortcuts, inject.NewlineMode(cfg.Sink.TypeNewlines))
		if err != nil {
			return nil, err
		}
		if mode == Type || clipboardMode == clipboard.None {
			target = sinkFunc(injector.Type)
		} else {
			target = sinkFunc(injector.Paste)
		}
	case Clipboard:
		target = sinkFunc(func(context.Context, string) error { return nil })
	case Stdout:
		writer := options.Stdout
		if writer == nil {
			writer = os.Stdout
		}
		target = &stdoutSink{writer: writer}
	case File:
		if strings.TrimSpace(cfg.Sink.File) == "" {
			return nil, errs.New(errs.ConfigInvalid, "sink", "un chemin est obligatoire pour le sink file")
		}
		target = &fileSink{path: cfg.Sink.File}
	default:
		return nil, errs.New(errs.ConfigInvalid, "sink", fmt.Sprintf("mode invalide: %q", mode))
	}

	return Once(&clipboardSink{clipboard: board, target: target}), nil
}

type sinkFunc func(context.Context, string) error

func (f sinkFunc) Deliver(ctx context.Context, text string) error { return f(ctx, text) }

type stdoutSink struct {
	writer io.Writer
}

func (s *stdoutSink) Deliver(_ context.Context, text string) error {
	if _, err := fmt.Fprintln(s.writer, text); err != nil {
		return errs.Wrap(errs.SinkFailed, "sink-stdout", "écriture sur stdout impossible", err)
	}
	return nil
}

type fileSink struct {
	path string
}

func (s *fileSink) Deliver(_ context.Context, text string) error {
	if err := xdg.WriteFileAtomic(s.path, []byte(text), 0o600); err != nil {
		return errs.Wrap(errs.SinkFailed, "sink-file", "écriture atomique impossible", err)
	}
	return nil
}

type clipboardSink struct {
	clipboard *clipboard.Clipboard
	target    Sink
}

func (s *clipboardSink) CopyRaw(ctx context.Context, text string) error {
	return s.clipboard.CopyRaw(ctx, text)
}

func (s *clipboardSink) Deliver(ctx context.Context, text string) error {
	if err := s.clipboard.CopyFinal(ctx, text); err != nil {
		return err
	}
	err := s.target.Deliver(ctx, text)
	s.clipboard.Clear(ctx)
	return err
}

type onceSink struct {
	once sync.Once
	sink Sink
	err  error
}

func (s *onceSink) CopyRaw(ctx context.Context, text string) error {
	target, ok := s.sink.(interface {
		CopyRaw(context.Context, string) error
	})
	if !ok {
		return nil
	}
	return target.CopyRaw(ctx, text)
}

// Once returns a concurrency-safe sink that invokes its target at most once.
// Every caller observes the first delivery's result.
func Once(target Sink) Sink {
	return &onceSink{sink: target}
}

func (s *onceSink) Deliver(ctx context.Context, text string) error {
	s.once.Do(func() {
		s.err = s.sink.Deliver(ctx, text)
	})
	return s.err
}
