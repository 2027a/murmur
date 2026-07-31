package session

import (
	"context"
	"sync"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/ipc"
)

type BootstrapOptions struct {
	Context   context.Context
	SessionID string
	Bus       *events.Bus
	Now       func() time.Time
}

// Bootstrap exposes the starting state while session dependencies are built.
// Once attached, commands are delegated to the orchestrator sharing its machine.
type Bootstrap struct {
	mu        sync.Mutex
	machine   *Machine
	ctx       context.Context
	cancel    context.CancelFunc
	startedAt time.Time
	now       func() time.Time
	target    ipc.Handler
	provider  string
}

func NewBootstrap(options BootstrapOptions) (*Bootstrap, error) {
	if options.SessionID == "" {
		return nil, errs.New(errs.ConfigInvalid, "session", "identifiant de session manquant")
	}
	if options.Context == nil {
		options.Context = context.Background()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	ctx, cancel := context.WithCancel(options.Context)
	machine := NewMachine(options.SessionID, eventPublisher{bus: options.Bus})
	machine.now = options.Now
	b := &Bootstrap{
		machine: machine, ctx: ctx, cancel: cancel,
		startedAt: options.Now().UTC(), now: options.Now,
	}
	if err := machine.Apply(StartCommand); err != nil {
		cancel()
		return nil, err
	}
	return b, nil
}

func (b *Bootstrap) Context() context.Context { return b.ctx }

func (b *Bootstrap) Attach(orchestrator *Orchestrator) {
	b.mu.Lock()
	b.target = orchestrator
	b.mu.Unlock()
}

func (b *Bootstrap) Handle(ctx context.Context, request ipc.Request) ipc.Response {
	b.mu.Lock()
	if b.target != nil {
		target := b.target
		b.mu.Unlock()
		return target.Handle(ctx, request)
	}
	defer b.mu.Unlock()

	switch request.Command {
	case ipc.Status:
		return ipc.Success(string(b.machine.State()))
	case ipc.Stop, ipc.Toggle:
		command := StopCommand
		if request.Command == ipc.Toggle {
			command = ToggleCommand
		}
		if err := b.machine.Apply(command); err != nil {
			return ipc.Failure(err)
		}
		return ipc.Success(string(b.machine.State()))
	case ipc.Cancel:
		if b.machine.Result() == "" {
			if err := b.machine.ApplyTerminal(CancelCommand, b.provider, errs.Cancelled); err != nil {
				return ipc.Failure(err)
			}
		}
		b.cancel()
		return ipc.Success(string(b.machine.State()))
	default:
		return ipc.Failure(errs.New(errs.ConfigInvalid, "session", "commande IPC non gérée"))
	}
}

func (b *Bootstrap) Finish(err error, provider string) LastResult {
	if provider == "" {
		b.mu.Lock()
		provider = b.provider
		b.mu.Unlock()
	}
	if b.machine.Result() == "" {
		if errs.Classify(err) == errs.Cancelled {
			_ = b.machine.ApplyTerminal(CancelCommand, provider, errs.Cancelled)
		} else {
			_ = b.machine.ApplyTerminal(FailCommand, provider, errs.Classify(err))
		}
	}
	result := LastResult{
		Version: 1, SessionID: b.machine.sessionID, Result: b.machine.Result(),
		StartedAt: b.startedAt, FinishedAt: b.now().UTC(), Provider: provider,
	}
	if result.Result == Failed && err != nil {
		result.Error = resultError(err)
	}
	b.cancel()
	return result
}

func (b *Bootstrap) setBuildingProvider(provider string) {
	b.mu.Lock()
	b.provider = provider
	b.mu.Unlock()
}

func (b *Bootstrap) machineOptions() (*Machine, time.Time) {
	return b.machine, b.startedAt
}

var _ ipc.Handler = (*Bootstrap)(nil)
