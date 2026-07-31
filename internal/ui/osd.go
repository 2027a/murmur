package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

const (
	DefaultOSDHelper      = "murmur-osd"
	DefaultOSDMaxRestarts = 3
)

type OSDPayload struct {
	Version    int        `json:"v"`
	State      string     `json:"state"`
	Visible    bool       `json:"visible"`
	Result     string     `json:"result,omitempty"`
	ErrorClass errs.Class `json:"error_class,omitempty"`
	Provider   string     `json:"provider,omitempty"`
	Partial    int        `json:"partial,omitempty"`
	Total      int        `json:"total,omitempty"`
}

type OSDOptions struct {
	Helper      string
	Args        []string
	Buffer      int
	MaxRestarts int
	Warn        WarnFunc
}

type OSD struct {
	executor sysexec.Executor
	options  OSDOptions
	queue    chan OSDPayload
	ctx      context.Context
	cancel   context.CancelFunc
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once

	mu     sync.Mutex
	closed bool
	proc   sysexec.Process
}

func NewOSD(executor sysexec.Executor, options OSDOptions) *OSD {
	if options.Helper == "" {
		options.Helper = DefaultOSDHelper
	}
	if options.Buffer < 1 {
		options.Buffer = 16
	}
	if options.MaxRestarts == 0 {
		options.MaxRestarts = DefaultOSDMaxRestarts
	} else if options.MaxRestarts < 0 {
		options.MaxRestarts = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	u := &OSD{
		executor: executor,
		options:  options,
		queue:    make(chan OSDPayload, options.Buffer),
		ctx:      ctx,
		cancel:   cancel,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go u.run()
	return u
}

func (u *OSD) Show(event events.Event) {
	if u == nil || u.executor == nil {
		return
	}
	payload, ok := OSDPayloadFromEvent(event)
	if !ok {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	select {
	case u.queue <- payload:
	default:
		select {
		case <-u.queue:
		default:
		}
		select {
		case u.queue <- payload:
		default:
		}
	}
}

func (u *OSD) Close() {
	if u == nil {
		return
	}
	u.once.Do(func() {
		u.mu.Lock()
		u.closed = true
		proc := u.proc
		u.mu.Unlock()
		close(u.stop)
		u.cancel()
		if proc != nil {
			_ = proc.Kill()
		}
		<-u.done
	})
}

func (u *OSD) run() {
	defer close(u.done)
	var processDone <-chan error
	var lastPayload OSDPayload
	haveLastPayload := false
	starts := 0
	for {
		select {
		case <-u.stop:
			u.killProcess()
			return
		case err := <-processDone:
			u.clearProcess()
			processDone = nil
			if err != nil {
				warning(u.options.Warn, "helper OSD termine: %v", err)
			}
			if !haveLastPayload || starts >= 1+u.options.MaxRestarts {
				continue
			}
			starts++
			proc, nextDone, startErr := u.startProcess()
			if startErr != nil {
				warning(u.options.Warn, "redemarrage du helper OSD ignore: %v", startErr)
				continue
			}
			processDone = nextDone
			if writeErr := writeOSDPayload(proc, lastPayload); writeErr != nil {
				warning(u.options.Warn, "ecriture OSD ignoree: %v", writeErr)
				_ = proc.Kill()
				u.clearProcess()
				processDone = nil
			}
		case payload := <-u.queue:
			lastPayload = payload
			haveLastPayload = true
			proc := u.currentProcess()
			if proc == nil {
				if starts >= 1+u.options.MaxRestarts {
					continue
				}
				starts++
				var err error
				proc, processDone, err = u.startProcess()
				if err != nil {
					warning(u.options.Warn, "demarrage du helper OSD ignore: %v", err)
					continue
				}
			}
			if err := writeOSDPayload(proc, payload); err != nil {
				warning(u.options.Warn, "ecriture OSD ignoree: %v", err)
				_ = proc.Kill()
				u.clearProcess()
				processDone = nil
			}
		}
	}
}

func writeOSDPayload(proc sysexec.Process, payload OSDPayload) error {
	stdin, ok := proc.(sysexec.InputProcess)
	if !ok {
		return errors.New("helper sans entree standard")
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = stdin.Stdin().Write(line)
	return err
}

func (u *OSD) startProcess() (sysexec.Process, <-chan error, error) {
	proc, err := u.executor.Start(u.ctx, u.options.Helper, append([]string(nil), u.options.Args...))
	if err != nil {
		return nil, nil, err
	}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		_ = proc.Kill()
		return nil, nil, errors.New("OSD ferme")
	}
	u.proc = proc
	u.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	return proc, done, nil
}

func (u *OSD) currentProcess() sysexec.Process {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.proc
}

func (u *OSD) clearProcess() {
	u.mu.Lock()
	u.proc = nil
	u.mu.Unlock()
}

func (u *OSD) killProcess() {
	u.mu.Lock()
	proc := u.proc
	u.proc = nil
	u.mu.Unlock()
	if proc != nil {
		if stdin, ok := proc.(interface{ Stdin() io.WriteCloser }); ok {
			_ = stdin.Stdin().Close()
		}
		_ = proc.Kill()
	}
}

func OSDPayloadFromEvent(event events.Event) (OSDPayload, bool) {
	payload := OSDPayload{
		Version:    1,
		State:      event.State,
		Visible:    true,
		Result:     event.Result,
		ErrorClass: event.ErrorClass,
		Provider:   event.Provider,
		Partial:    event.Partial,
		Total:      event.Total,
	}
	if event.Type == events.Terminal {
		payload.State = "terminal"
		return payload, true
	}
	if event.Type == events.Delivered {
		payload.State = "delivering"
		return payload, true
	}
	if event.Type == events.Degraded || event.Type == events.PartialFailed {
		payload.State = "error"
		return payload, true
	}
	if payload.State != "" {
		return payload, true
	}
	return OSDPayload{}, false
}
