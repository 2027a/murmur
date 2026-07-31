package textpipe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/plugin"
)

type Warning struct {
	Class     errs.Class `json:"class"`
	Component string     `json:"component"`
	Message   string     `json:"message"`
}

const (
	warningCleanupDegraded = "cleanup dégradé: texte précédent conservé"
	warningCleanupInvalid  = "cleanup dégradé: sortie invalide rejetée"
	warningPluginDegraded  = "plugin dégradé: texte précédent conservé"
	warningPluginInvalid   = "plugin dégradé: sortie invalide rejetée"
)

type Result struct {
	Text     string    `json:"text"`
	Warnings []Warning `json:"warnings,omitempty"`
}

// Text and Warning are the generic contract for optional future pipeline stages.
type Text struct {
	Content    string
	Protection Protection
	Warnings   []Warning
}

type Stage interface {
	Name() string
	Apply(context.Context, Text) (Text, error)
}

type Cleanup interface {
	Cleanup(context.Context, string, Protection) (string, error)
}

type CleanupFunc func(context.Context, string, Protection) (string, error)

func (f CleanupFunc) Cleanup(ctx context.Context, text string, protection Protection) (string, error) {
	return f(ctx, text, protection)
}

type Plugin = plugin.Plugin
type PluginFunc = plugin.Func

type Options struct {
	CommandPack string
	STTLanguage string
	ASCIIQuotes bool
	Cleanup     Cleanup
	Plugin      Plugin
}

type Pipeline struct {
	pack        *Pack
	asciiQuotes bool
	cleanup     Cleanup
	plugin      Plugin
	protector   *Protector
}

func New(options Options) (*Pipeline, error) {
	pack, err := LoadPack(options.CommandPack, options.STTLanguage)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		pack:        pack,
		asciiQuotes: options.ASCIIQuotes,
		cleanup:     options.Cleanup,
		plugin:      options.Plugin,
		protector:   NewProtector(rand.Reader),
	}, nil
}

// Run processes one already assembled transcript. Consequently cleanup is
// global and invoked at most once, regardless of the transcript's segment count.
func (p *Pipeline) Run(ctx context.Context, transcript string) (Result, error) {
	if p == nil || p.pack == nil {
		return Result{}, errors.New("text pipeline is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	protected, err := p.protector.Protect(transcript, p.pack)
	if err != nil {
		return Result{}, fmt.Errorf("protect voice commands: %w", err)
	}

	candidate := protected.Text
	var warnings []Warning
	if p.cleanup != nil {
		cleaned, cleanupErr := p.cleanup.Cleanup(ctx, candidate, protected.Protection.clone())
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		switch {
		case cleanupErr != nil:
			warnings = append(warnings, Warning{Class: errs.Classify(cleanupErr), Component: "cleanup", Message: warningCleanupDegraded})
		case candidate != "" && strings.TrimSpace(cleaned) == "":
			warnings = append(warnings, Warning{Class: errs.EmptyResponse, Component: "cleanup", Message: warningCleanupInvalid})
		case protected.Protection.Validate(cleaned) != nil:
			warnings = append(warnings, Warning{Class: errs.EmptyResponse, Component: "cleanup", Message: warningCleanupInvalid})
		default:
			candidate = cleaned
		}
	}

	rendered, err := Render(candidate, protected.Protection, p.pack, p.asciiQuotes)
	if err != nil {
		return Result{}, fmt.Errorf("render voice commands: %w", err)
	}
	final := rendered
	if p.plugin != nil {
		transformed, pluginErr := p.plugin.Transform(ctx, rendered)
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		switch {
		case pluginErr != nil:
			warnings = append(warnings, Warning{Class: errs.Classify(pluginErr), Component: "plugin", Message: warningPluginDegraded})
		case rendered != "" && strings.TrimSpace(transformed) == "":
			warnings = append(warnings, Warning{Class: errs.EmptyResponse, Component: "plugin", Message: warningPluginInvalid})
		case strings.Contains(transformed, protected.Protection.Namespace):
			warnings = append(warnings, Warning{Class: errs.EmptyResponse, Component: "plugin", Message: warningPluginInvalid})
		default:
			final = transformed
		}
	}
	return Result{Text: final, Warnings: warnings}, nil
}
