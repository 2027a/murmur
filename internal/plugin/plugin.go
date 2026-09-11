package plugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/policy"
)

// Plugin is the runtime operation consumed by textpipe.
type Plugin interface {
	Name() string
	Zone() policy.Zone
	Transform(context.Context, string) (string, error)
}

// Definition declares the identity and furthest trust zone of an implementation.
type Definition interface {
	Name() string
	Zone() policy.Zone
}

// Local is the deliberately small contract for implementations without network I/O.
type Local interface {
	Definition
	Transform(context.Context, string) (string, error)
}

// Func adapts an embedded local function to Local. Production plugins are
// still wrapped with New to enforce their configured timeout and policy.
type Func func(context.Context, string) (string, error)

func (Func) Name() string      { return "function" }
func (Func) Zone() policy.Zone { return policy.Local }
func (f Func) Transform(ctx context.Context, text string) (string, error) {
	return f(ctx, text)
}

type Options struct {
	Timeout      time.Duration
	AllowedZones []policy.Zone
}

type result struct {
	text string
	err  error
}

type bounded struct {
	definition Definition
	local      Local
	timeout    time.Duration
}

// New validates a plugin declaration and returns a timeout-bounded runtime
// adapter. Plugin errors are intentionally left recoverable by textpipe.
func New(implementation Definition, options Options) (Plugin, error) {
	if implementation == nil {
		return nil, errs.New(errs.ConfigInvalid, "plugin", "implémentation absente")
	}
	if strings.TrimSpace(implementation.Name()) == "" {
		return nil, errs.New(errs.ConfigInvalid, "plugin", "nom absent")
	}
	if options.Timeout <= 0 {
		return nil, errs.New(errs.ConfigInvalid, "plugin", "timeout non positif")
	}
	zone := implementation.Zone()
	if zone < policy.Local || zone > policy.Internet {
		return nil, errs.New(errs.ConfigInvalid, "plugin", "zone de confiance invalide")
	}
	if !zoneAllowed(zone, options.AllowedZones) {
		return nil, errs.New(errs.PolicyBlocked, "plugin", fmt.Sprintf("zone %s interdite par policy.plugin_zones", zone))
	}
	adapter := &bounded{definition: implementation, timeout: options.Timeout}
	if zone != policy.Local {
		return nil, errs.New(errs.ConfigInvalid, "plugin", "seuls les plugins locaux sont pris en charge")
	}
	local, ok := implementation.(Local)
	if !ok {
		return nil, errs.New(errs.ConfigInvalid, "plugin", "un plugin local doit implémenter plugin.Local")
	}
	adapter.local = local
	return adapter, nil
}

func (p *bounded) Name() string      { return p.definition.Name() }
func (p *bounded) Zone() policy.Zone { return p.definition.Zone() }

func (p *bounded) Transform(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	done := make(chan result, 1)
	go func() {
		var transformed string
		var err error
		defer func() {
			if recover() != nil {
				err = errs.New(errs.Internal, "plugin/"+p.Name(), "panic récupérée")
			}
			done <- result{text: transformed, err: err}
		}()
		transformed, err = p.local.Transform(callCtx, text)
	}()

	select {
	case transformed := <-done:
		if transformed.err == nil {
			return transformed.text, nil
		}
		class := errs.Classify(transformed.err)
		return "", errs.Wrap(class, "plugin/"+p.Name(), "transformation impossible", transformed.err)
	case <-callCtx.Done():
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", errs.New(errs.Timeout, "plugin/"+p.Name(), "timeout dépassé")
	}
}

func zoneAllowed(zone policy.Zone, allowed []policy.Zone) bool {
	for _, candidate := range allowed {
		if candidate == zone {
			return true
		}
	}
	return false
}

// Plain is the explicit identity implementation.
type Plain struct{}

func (Plain) Name() string      { return "plain" }
func (Plain) Zone() policy.Zone { return policy.Local }
func (Plain) Transform(_ context.Context, text string) (string, error) {
	return text, nil
}
