package sysexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Process interface {
	Signal(os.Signal) error
	Wait() error
	Kill() error
}

type InputProcess interface {
	Process
	Stdin() io.WriteCloser
}

type Executor interface {
	Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error)
	Start(ctx context.Context, name string, args []string) (Process, error)
}

type LimitedExecutor interface {
	Executor
	RunLimited(ctx context.Context, name string, args []string, stdin []byte, maxOutput int64) ([]byte, error)
}

type DiscardExecutor interface {
	Executor
	RunDiscard(ctx context.Context, name string, args []string, stdin []byte) error
}

var ErrOutputLimit = errors.New("sortie du processus trop volumineuse")

type OSExecutor struct {
	// Env is the complete child environment. A nil value inherits the current
	// process environment; a non-nil empty slice provides an empty environment.
	Env []string
}

func New() Executor { return OSExecutor{} }

func (e OSExecutor) Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	var output limitedBuffer
	if err := e.run(ctx, name, args, stdin, &output); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (e OSExecutor) RunLimited(ctx context.Context, name string, args []string, stdin []byte, maxOutput int64) ([]byte, error) {
	output := limitedBuffer{limit: maxOutput}
	if err := e.run(ctx, name, args, stdin, &output); err != nil {
		return nil, err
	}
	if output.exceeded {
		return nil, ErrOutputLimit
	}
	return output.Bytes(), nil
}

func (e OSExecutor) RunDiscard(ctx context.Context, name string, args []string, stdin []byte) error {
	return e.run(ctx, name, args, stdin, io.Discard)
}

func (e OSExecutor) run(ctx context.Context, name string, args []string, stdin []byte, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	e.setEnv(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = stdout
	return cmd.Run()
}

func (e OSExecutor) Start(ctx context.Context, name string, args []string) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(name, args...)
	e.setEnv(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	process := &osProcess{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = process.Kill()
		case <-process.done:
		}
	}()
	return process, nil
}

func (e OSExecutor) setEnv(cmd *exec.Cmd) {
	if e.Env != nil {
		cmd.Env = append([]string{}, e.Env...)
	}
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.buffer.Write(p)
	}
	written := len(p)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.exceeded = true
		return written, nil
	}
	if int64(len(p)) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return written, nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buffer.Bytes() }

// RunLimited bounds stdout in production while retaining compatibility with
// injected executors used by callers and tests.
func RunLimited(ctx context.Context, executor Executor, name string, args []string, stdin []byte, maxOutput int64) ([]byte, error) {
	if limited, ok := executor.(LimitedExecutor); ok {
		return limited.RunLimited(ctx, name, args, stdin, maxOutput)
	}
	output, err := executor.Run(ctx, name, args, stdin)
	if err == nil && maxOutput > 0 && int64(len(output)) > maxOutput {
		return nil, ErrOutputLimit
	}
	return output, err
}

// RunDiscard executes a helper while discarding stdout for commands whose
// output is not part of their contract.
func RunDiscard(ctx context.Context, executor Executor, name string, args []string, stdin []byte) error {
	if discard, ok := executor.(DiscardExecutor); ok {
		return discard.RunDiscard(ctx, name, args, stdin)
	}
	_, err := executor.Run(ctx, name, args, stdin)
	return err
}

// FilterEnvironment returns a copy without any assignment whose name is in
// removed. It removes duplicate assignments too.
func FilterEnvironment(environment, removed []string) []string {
	blocked := make(map[string]struct{}, len(removed))
	for _, name := range removed {
		if name != "" {
			blocked[name] = struct{}{}
		}
	}
	filtered := make([]string, 0, len(environment))
	for _, assignment := range environment {
		name, _, _ := strings.Cut(assignment, "=")
		if _, remove := blocked[name]; remove {
			continue
		}
		filtered = append(filtered, assignment)
	}
	return filtered
}

type osProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	exited  bool
	waitErr error
}

func (p *osProcess) Stdin() io.WriteCloser { return p.stdin }

func (p *osProcess) Signal(signal os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return os.ErrProcessDone
	}
	if p.cmd.Process == nil {
		return os.ErrProcessDone
	}
	if value, ok := signal.(syscall.Signal); ok {
		err := syscall.Kill(-p.cmd.Process.Pid, value)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return p.cmd.Process.Signal(signal)
}

func (p *osProcess) Wait() error {
	p.once.Do(func() {
		err := p.cmd.Wait()
		p.mu.Lock()
		p.waitErr = err
		p.exited = true
		close(p.done)
		p.mu.Unlock()
	})
	<-p.done
	return p.waitErr
}

func (p *osProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return os.ErrProcessDone
	}
	return killProcessGroup(p.cmd.Process)
}

func killProcessGroup(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

var _ Executor = OSExecutor{}
var _ LimitedExecutor = OSExecutor{}
var _ InputProcess = (*osProcess)(nil)
