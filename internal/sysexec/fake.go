package sysexec

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
)

type Call struct {
	Name  string
	Args  []string
	Stdin []byte
}

type FakeExecutor struct {
	RunFunc   func(context.Context, string, []string, []byte) ([]byte, error)
	StartFunc func(context.Context, string, []string) (Process, error)

	mu    sync.Mutex
	calls []Call
}

func (f *FakeExecutor) Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	f.record(Call{Name: name, Args: args, Stdin: stdin})
	if f.RunFunc == nil {
		return nil, nil
	}
	return f.RunFunc(ctx, name, args, stdin)
}

func (f *FakeExecutor) RunDiscard(ctx context.Context, name string, args []string, stdin []byte) error {
	_, err := f.Run(ctx, name, args, stdin)
	return err
}

func (f *FakeExecutor) Start(ctx context.Context, name string, args []string) (Process, error) {
	f.record(Call{Name: name, Args: args})
	if f.StartFunc == nil {
		return NewFakeProcess(), nil
	}
	return f.StartFunc(ctx, name, args)
}

func (f *FakeExecutor) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([]Call, len(f.calls))
	for i, call := range f.calls {
		calls[i] = Call{
			Name:  call.Name,
			Args:  append([]string(nil), call.Args...),
			Stdin: append([]byte(nil), call.Stdin...),
		}
	}
	return calls
}

func (f *FakeExecutor) record(call Call) {
	call.Args = append([]string(nil), call.Args...)
	call.Stdin = append([]byte(nil), call.Stdin...)
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

type FakeProcess struct {
	SignalFunc func(os.Signal) error
	KillFunc   func() error

	mu      sync.Mutex
	done    chan struct{}
	stdin   *fakeProcessStdin
	waitErr error
	signals []os.Signal
	kills   int
}

func NewFakeProcess() *FakeProcess {
	return &FakeProcess{done: make(chan struct{}), stdin: &fakeProcessStdin{}}
}

func (p *FakeProcess) Stdin() io.WriteCloser { return p.stdin }

func (p *FakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	fn := p.SignalFunc
	p.mu.Unlock()
	if fn != nil {
		return fn(sig)
	}
	return nil
}

func (p *FakeProcess) Kill() error {
	p.mu.Lock()
	p.kills++
	fn := p.KillFunc
	p.mu.Unlock()
	if fn != nil {
		return fn()
	}
	p.Exit(nil)
	return nil
}

func (p *FakeProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *FakeProcess) Exit(err error) {
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return
	default:
	}
	p.waitErr = err
	close(p.done)
	p.mu.Unlock()
}

func (p *FakeProcess) Signals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

func (p *FakeProcess) KillCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kills
}

var _ Executor = (*FakeExecutor)(nil)
var _ Process = (*FakeProcess)(nil)

type fakeProcessStdin struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	closed bool
}

func (w *fakeProcessStdin) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return w.buffer.Write(value)
}

func (w *fakeProcessStdin) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return nil
}
