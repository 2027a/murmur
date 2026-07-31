package ui

import (
	"git.2027a.net/mathieu/murmur/internal/events"
	"git.2027a.net/mathieu/murmur/internal/logging"
)

type UI interface {
	Show(events.Event)
	Close()
}

type WarnFunc func(format string, args ...any)

func warning(warn WarnFunc, format string, args ...any) {
	if warn != nil {
		warn(format, args...)
		return
	}
	logging.Warnf("ui: "+format, args...)
}

type None struct{}

func NewNone() UI { return None{} }

func (None) Show(events.Event) {}

func (None) Close() {}
