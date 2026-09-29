package ui

import (
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/logging"
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

// Background STT must not replace the microphone indicator or restart its
// timeout every few seconds. Failures remain observable in the journal and in
// the final fallback/degraded outcome.
func backgroundProgress(event events.Event) bool {
	if event.Type == events.TimingRecorded {
		return true
	}
	if event.State != "recording" && event.State != "stopping" {
		return false
	}
	return event.Type == events.PartialStarted || event.Type == events.PartialDone || event.Type == events.PartialFailed
}

func NewNone() UI { return None{} }

func (None) Show(events.Event) {}

func (None) Close() {}
