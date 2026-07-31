package ui

import (
	"context"
	"sync"

	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

type Sound struct {
	next     UI
	executor sysexec.Executor
	file     string
	warn     WarnFunc
	triggers chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
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
		triggers: make(chan struct{}, 1),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *Sound) Show(event events.Event) {
	s.next.Show(event)
	if event.Type == events.CaptureReady {
		s.Play()
	}
}

func (s *Sound) Play() {
	if s == nil || s.executor == nil {
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
		s.cancel()
		<-s.done
		s.next.Close()
	})
}

func (s *Sound) run() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.triggers:
			if !s.playOnce() {
				warning(s.warn, "aucun lecteur sonore disponible")
			}
		}
	}
}

func (s *Sound) playOnce() bool {
	file := s.file
	if file == "" {
		file = "/usr/share/sounds/freedesktop/stereo/message.oga"
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
		if _, err := s.executor.Run(s.ctx, command.name, command.args, nil); err == nil {
			return true
		}
	}
	return false
}
