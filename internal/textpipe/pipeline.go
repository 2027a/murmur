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
	Provider  string     `json:"provider,omitempty"`
	Component string     `json:"component"`
	Message   string     `json:"message"`
}

const (
	warningCleanupDegraded = "cleanup dégradé: texte précédent conservé"
	warningCleanupInvalid  = "cleanup dégradé: sortie invalide rejetée"
	warningCleanupBudget   = "cleanup dégradé: sortie supérieure au budget texte rejetée"
	warningPluginDegraded  = "plugin dégradé: texte précédent conservé"
	warningPluginInvalid   = "plugin dégradé: sortie invalide rejetée"
	warningPluginBudget    = "plugin dégradé: sortie supérieure au budget texte rejetée"
	defaultMaxTextBytes    = 16 << 20
)

type Result struct {
	Text     string    `json:"text"`
	Warnings []Warning `json:"warnings,omitempty"`
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
	CommandPack     string
	STTLanguage     string
	ASCIIQuotes     bool
	CleanupProvider string
	Cleanup         Cleanup
	Plugin          Plugin
	MaxTextBytes    int
}

type Pipeline struct {
	pack            *Pack
	asciiQuotes     bool
	cleanupProvider string
	cleanup         Cleanup
	plugin          Plugin
	protector       *Protector
	maxTextBytes    int
}

func New(options Options) (*Pipeline, error) {
	if options.MaxTextBytes < 0 {
		return nil, errs.New(errs.ConfigInvalid, "textpipe", "budget texte invalide")
	}
	if options.MaxTextBytes == 0 {
		options.MaxTextBytes = defaultMaxTextBytes
	}
	pack, err := LoadPack(options.CommandPack, options.STTLanguage)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		pack:            pack,
		asciiQuotes:     options.ASCIIQuotes,
		cleanupProvider: options.CleanupProvider,
		cleanup:         options.Cleanup,
		plugin:          options.Plugin,
		protector:       NewProtector(rand.Reader),
		maxTextBytes:    options.MaxTextBytes,
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
	if err := p.requireBudget(transcript, "transcription"); err != nil {
		return Result{}, err
	}
	protected, err := p.protector.Protect(transcript, p.pack)
	if err != nil {
		return Result{}, fmt.Errorf("protect voice commands: %w", err)
	}
	if err := p.requireBudget(protected.Text, "protection des commandes vocales"); err != nil {
		return Result{}, err
	}

	candidate := protected.Text
	var warnings []Warning
	if p.cleanup != nil {
		cleaned, cleanupErr := p.cleanup.Cleanup(ctx, candidate, protected.Protection.clone())
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if warning, ok := optionalWarning(cleanupErr, "cleanup", warningCleanupDegraded); ok {
			warning.Provider = p.cleanupProvider
			warnings = append(warnings, warning)
		} else if len(cleaned) > p.maxTextBytes {
			warnings = append(warnings, Warning{Class: errs.ServerError, Provider: p.cleanupProvider, Component: "cleanup", Message: warningCleanupBudget})
		} else if warning, ok := validateOptionalText(cleaned, candidate, protected.Protection, true, "cleanup", warningCleanupInvalid); ok {
			warning.Provider = p.cleanupProvider
			warnings = append(warnings, warning)
		} else {
			candidate = cleaned
		}
	}

	rendered, err := Render(candidate, protected.Protection, p.pack, p.asciiQuotes)
	if err != nil {
		return Result{}, fmt.Errorf("render voice commands: %w", err)
	}
	if err := p.requireBudget(rendered, "rendu des commandes vocales"); err != nil {
		return Result{}, err
	}
	final := rendered
	if p.plugin != nil {
		transformed, pluginErr := p.plugin.Transform(ctx, rendered)
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if warning, ok := optionalWarning(pluginErr, "plugin", warningPluginDegraded); ok {
			warnings = append(warnings, warning)
		} else if len(transformed) > p.maxTextBytes {
			warnings = append(warnings, Warning{Class: errs.ServerError, Component: "plugin", Message: warningPluginBudget})
		} else if warning, ok := validateOptionalText(transformed, rendered, protected.Protection, false, "plugin", warningPluginInvalid); ok {
			warnings = append(warnings, warning)
		} else {
			final = transformed
		}
	}
	return Result{Text: final, Warnings: warnings}, nil
}

func (p *Pipeline) requireBudget(text, stage string) error {
	if len(text) > p.maxTextBytes {
		return errs.New(errs.ServerError, "textpipe", stage+" supérieure au budget texte configuré")
	}
	return nil
}

func optionalWarning(err error, component, degraded string) (Warning, bool) {
	if err == nil {
		return Warning{}, false
	}
	return Warning{Class: errs.Classify(err), Component: component, Message: degraded}, true
}

func validateOptionalText(candidate, previous string, protection Protection, requireProtectedCommands bool, component, message string) (Warning, bool) {
	if previous != "" && strings.TrimSpace(candidate) == "" {
		return Warning{Class: errs.EmptyResponse, Component: component, Message: message}, true
	}
	if !requireProtectedCommands && protection.Namespace != "" && strings.Contains(candidate, protection.Namespace) {
		return Warning{Class: errs.EmptyResponse, Component: component, Message: message}, true
	}
	if requireProtectedCommands && protection.HasCommands() {
		if err := protection.Validate(candidate); err != nil {
			return Warning{Class: errs.EmptyResponse, Component: component, Message: message}, true
		}
	}
	return Warning{}, false
}
