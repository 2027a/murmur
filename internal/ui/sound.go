package ui

import (
	"context"
	"sync"
	"time"

	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/logging"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

const (
	soundPlaybackTimeout = 500 * time.Millisecond
	soundCloseTimeout    = 2 * time.Second
)

type Sound struct {
	next      UI
	executor  sysexec.Executor
	file      string
	warn      WarnFunc
	triggers  chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	shutdown  chan struct{}
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	recording bool
	closed    bool
}

func NewSound(next UI, executor sysexec.Executor, file string, warn WarnFunc) *Sound {
	if next == nil {
		next = NewNone()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sound{
		next:     next,
		executor: executor,
		file:     file,
		warn:     warn,
		triggers: make(chan struct{}, 2),
		ctx:      ctx,
		cancel:   cancel,
		shutdown: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *Sound) Show(event events.Event) {
	s.next.Show(event)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	play := false
	switch {
	case event.Type == events.CaptureReady && !s.recording:
		s.recording = true
		play = true
	case s.recording && event.Type == events.StateChanged && event.State == "stopping":
		s.recording = false
		play = true
	case s.recording && event.Type == events.Terminal:
		s.recording = false
		play = true
	}
	if play {
		s.enqueueLocked()
	}
}

func (s *Sound) Play() {
	if s == nil || s.executor == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.enqueueLocked()
}

func (s *Sound) enqueueLocked() {
	if s.executor == nil {
		return
	}
	select {
	case s.triggers <- struct{}{}:
	default:
	}
}

func (s *Sound) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.shutdown)
		timer := time.NewTimer(soundCloseTimeout)
		select {
		case <-s.done:
			timer.Stop()
		case <-timer.C:
			s.cancel()
			<-s.done
		}
		s.cancel()
		s.next.Close()
	})
}

func (s *Sound) run() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.shutdown:
			for {
				select {
				case <-s.triggers:
					s.play()
				default:
					return
				}
			}
		case <-s.triggers:
			s.play()
		}
	}
}

func (s *Sound) play() {
	if !s.playOnce() {
		warning(logging.ComponentUI, s.warn, "aucun lecteur sonore disponible")
	}
}

func (s *Sound) playOnce() bool {
	file := s.file
	if file == "" {
		file = "/usr/share/sounds/freedesktop/stereo/bell.oga"
	}
	commands := []struct {
		name string
		args []string
	}{
		{name: "pw-play", args: []string{file}},
		{name: "paplay", args: []string{file}},
		{name: "canberra-gtk-play", args: []string{"-f", file}},
	}
	for _, command := range commands {
		ctx, cancel := context.WithTimeout(s.ctx, soundPlaybackTimeout)
		_, err := s.executor.Run(ctx, command.name, command.args, nil)
		cancel()
		if err == nil {
			return true
		}
	}
	return false
}
