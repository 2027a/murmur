package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/logging"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

const (
	DefaultOSDHelper           = "murmur-osd"
	DefaultOSDMaxRestarts      = 3
	DefaultOSDCloseTimeout     = 5 * time.Second
	DefaultOSDIdleCloseTimeout = 250 * time.Millisecond
	OSDHelperExitGrace         = time.Second
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
	Helper           string
	Args             []string
	Buffer           int
	MaxRestarts      int
	CloseTimeout     time.Duration
	IdleCloseTimeout time.Duration
	Warn             WarnFunc
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

	mu              sync.Mutex
	closed          bool
	proc            sysexec.Process
	terminal        *OSDPayload
	terminalWritten bool
	closeDeadline   time.Time
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
	if options.CloseTimeout <= 0 {
		options.CloseTimeout = DefaultOSDCloseTimeout
	}
	if options.IdleCloseTimeout <= 0 {
		options.IdleCloseTimeout = DefaultOSDIdleCloseTimeout
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
	if u.terminal != nil && payload.State != "terminal" {
		return
	}
	if payload.State == "terminal" {
		terminal := payload
		u.terminal = &terminal
		u.terminalWritten = false
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
		timeout := u.options.IdleCloseTimeout
		if u.terminal != nil {
			timeout = u.options.CloseTimeout
		}
		u.closeDeadline = time.Now().Add(timeout)
		u.mu.Unlock()
		close(u.stop)
		timer := time.NewTimer(timeout)
		select {
		case <-u.done:
		case <-timer.C:
			u.cancel()
			u.killProcess()
			<-u.done
		}
		timer.Stop()
		u.cancel()
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
			u.shutdown(processDone)
			return
		case err := <-processDone:
			u.clearProcess()
			processDone = nil
			if err != nil {
				warning(logging.ComponentOSD, u.options.Warn, "helper OSD terminé en erreur")
			}
			if u.isClosed() {
				u.shutdown(nil)
				return
			}
			if !haveLastPayload || starts >= 1+u.options.MaxRestarts {
				continue
			}
			starts++
			proc, nextDone, startErr := u.startProcess(lastPayload.State == "terminal")
			if startErr != nil {
				warning(logging.ComponentOSD, u.options.Warn, "redémarrage du helper OSD échoué")
				continue
			}
			processDone = nextDone
			if writeErr := writeOSDPayload(proc, lastPayload); writeErr != nil {
				warning(logging.ComponentOSD, u.options.Warn, "écriture vers le helper OSD échouée")
				_ = proc.Kill()
			} else {
				u.markTerminalWritten(lastPayload)
			}
		case payload := <-u.queue:
			if u.isClosed() {
				u.shutdown(processDone)
				return
			}
			lastPayload = payload
			haveLastPayload = true
			proc := u.currentProcess()
			if proc == nil {
				if starts >= 1+u.options.MaxRestarts {
					continue
				}
				starts++
				var err error
				proc, processDone, err = u.startProcess(payload.State == "terminal")
				if err != nil {
					warning(logging.ComponentOSD, u.options.Warn, "démarrage du helper OSD échoué")
					continue
				}
			}
			if err := writeOSDPayload(proc, payload); err != nil {
				warning(logging.ComponentOSD, u.options.Warn, "écriture vers le helper OSD échouée")
				_ = proc.Kill()
			} else {
				u.markTerminalWritten(payload)
			}
		}
	}
}

func (u *OSD) isClosed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.closed
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

func (u *OSD) startProcess(allowClosed bool) (sysexec.Process, <-chan error, error) {
	proc, err := u.executor.Start(u.ctx, u.options.Helper, append([]string(nil), u.options.Args...))
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	u.mu.Lock()
	if u.closed && !allowClosed {
		u.mu.Unlock()
		_ = proc.Kill()
		<-done
		return nil, nil, errors.New("OSD ferme")
	}
	u.proc = proc
	u.mu.Unlock()
	return proc, done, nil
}

func (u *OSD) markTerminalWritten(payload OSDPayload) {
	if payload.State != "terminal" {
		return
	}
	u.mu.Lock()
	u.terminalWritten = true
	u.mu.Unlock()
}

func (u *OSD) shutdown(processDone <-chan error) {
	u.mu.Lock()
	terminal := u.terminal
	written := u.terminalWritten
	u.mu.Unlock()
	proc := u.currentProcess()
	// A completed Wait before our EOF is a premature helper death.
	if proc != nil && processDone != nil {
		select {
		case err := <-processDone:
			if err != nil {
				warning(logging.ComponentOSD, u.options.Warn, "helper OSD terminé en erreur")
			}
			u.clearProcess()
			proc = nil
			processDone = nil
		default:
		}
	}
	if terminal != nil && !written && proc != nil {
		if err := writeOSDPayload(proc, *terminal); err != nil {
			warning(logging.ComponentOSD, u.options.Warn, "écriture terminale vers le helper OSD échouée")
			_ = proc.Kill()
			if processDone != nil {
				<-processDone
			}
			u.clearProcess()
			proc = nil
			processDone = nil
		} else {
			u.markTerminalWritten(*terminal)
			written = true
		}
	}
	if terminal != nil && (!written || proc == nil) && u.terminalRetryAllowed() {
		var err error
		proc, processDone, err = u.startProcess(true)
		if err != nil {
			warning(logging.ComponentOSD, u.options.Warn, "démarrage final du helper OSD échoué")
			return
		}
		if err := writeOSDPayload(proc, *terminal); err != nil {
			warning(logging.ComponentOSD, u.options.Warn, "écriture terminale vers le helper OSD échouée")
			_ = proc.Kill()
		} else {
			u.markTerminalWritten(*terminal)
		}
	}
	if proc == nil {
		return
	}
	if stdin, ok := proc.(sysexec.InputProcess); ok {
		_ = stdin.Stdin().Close()
	} else {
		_ = proc.Kill()
	}
	if processDone != nil {
		<-processDone
	}
	u.clearProcess()
}

func (u *OSD) terminalRetryAllowed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ctx.Err() == nil && !u.closeDeadline.IsZero() && time.Now().Before(u.closeDeadline)
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
