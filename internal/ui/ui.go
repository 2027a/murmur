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

func warning(component logging.Component, warn WarnFunc, message string) {
	if warn != nil {
		warn("%s", message)
		return
	}
	logging.Warnf(component, "%s", message)
}

type None struct{}

func NewNone() UI { return None{} }

func (None) Show(events.Event) {}

func (None) Close() {}
