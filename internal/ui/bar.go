package ui

import (
	"context"
	"sync"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/logging"
)

// BarState is a complete, replaceable UI snapshot. Reconnection requires no
// replay, and frequent acoustic updates cannot evict a terminal state.
type BarState struct {
	Version    int       `json:"v"`
	Sequence   uint64    `json:"seq"`
	Probe      bool      `json:"probe,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	State      string    `json:"state"`
	Result     string    `json:"result,omitempty"`
	ErrorClass string    `json:"error_class,omitempty"`
	Warning    string    `json:"warning,omitempty"`
	Delivery   string    `json:"delivery,omitempty"`
	Cleanup    bool      `json:"cleanup"`
	CaptureMS  int64     `json:"capture_ms,omitempty"`
	FinishedMS int64     `json:"finished_ms,omitempty"`
	Level      wav.Level `json:"level"`
}

type Bar struct {
	mu       sync.Mutex
	state    BarState
	event    events.Event
	revision uint64
	closed   bool
	wake     chan struct{}
	done     chan struct{}
	once     sync.Once
	path     string
	fallback UI
	warn     WarnFunc
}

func NewBar(path, delivery string, cleanup bool, fallback UI, warn WarnFunc) *Bar {
	b := &Bar{path: path, fallback: fallback, warn: warn, wake: make(chan struct{}, 1), done: make(chan struct{}),
		state: BarState{Version: 1, State: "idle", Delivery: delivery, Cleanup: cleanup}}
	go b.run()
	return b
}

func (b *Bar) Show(e events.Event) {
	if e.Type != events.SessionStarted && e.Type != events.CaptureReady && e.Type != events.StateChanged && e.Type != events.Degraded && e.Type != events.Terminal {
		return
	}
	b.mu.Lock()
	if b.closed || b.state.Result != "" {
		b.mu.Unlock()
		return
	}
	b.state.SessionID = e.SessionID
	if e.Type == events.Degraded {
		b.state.Warning = e.Component + ": " + string(e.ErrorClass)
	} else {
		b.state.State = e.State
		b.event = e
	}
	if e.Type == events.CaptureReady {
		b.state.CaptureMS = e.Timestamp.UnixMilli()
	}
	if e.Type == events.Terminal {
		b.state.Result = e.Result
		b.state.ErrorClass = string(e.ErrorClass)
		b.state.FinishedMS = e.Timestamp.UnixMilli()
	}
	if b.state.State != "recording" {
		b.state.Level = wav.Level{}
	}
	b.revision++
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Bar) Meter(level wav.Level) {
	b.mu.Lock()
	if !b.closed && b.state.State == "recording" && b.state.Result == "" {
		b.state.Level = level
	}
	b.mu.Unlock()
}

func (b *Bar) Close() {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		select {
		case b.wake <- struct{}{}:
		default:
		}
		<-b.done
	})
}

func (b *Bar) run() {
	defer close(b.done)
	if b.fallback != nil {
		defer b.fallback.Close()
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var sent, notified uint64
	var lastAttempt time.Time
	var available, attempted, warned bool
	for {
		select {
		case <-b.wake:
		case <-ticker.C:
		}
		b.mu.Lock()
		state, event, revision, closed := b.state, b.event, b.revision, b.closed
		b.mu.Unlock()
		if state.SessionID == "" {
			if closed {
				return
			}
			continue
		}
		// Heartbeats detect a lost shell even during a long, quiet cleanup.
		interval := 250 * time.Millisecond
		if available && state.State == "recording" {
			interval = 0 // The 50 ms ticker already bounds acoustic refreshes.
		}
		if !closed && sent == revision && time.Since(lastAttempt) < interval {
			continue
		}
		lastAttempt = time.Now()
		err := sendBarState(context.Background(), b.path, state)
		sent = revision
		if err == nil {
			if attempted && !available {
				if dismiss, ok := b.fallback.(interface{ Dismiss() }); ok {
					dismiss.Dismiss()
				}
			}
			available = true
		} else {
			if b.fallback != nil && (available || !attempted || notified != revision) {
				b.fallback.Show(event)
				notified = revision
			} else if b.fallback == nil && !warned {
				warning(logging.ComponentUI, b.warn, "module QuickShell indisponible (ui.mode=quickshell)")
				warned = true
			}
			available = false
		}
		attempted = true
		if closed {
			return
		}
	}
}
