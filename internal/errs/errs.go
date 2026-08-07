package errs

import (
	"context"
	"errors"
	"fmt"
)

type Class string

const (
	ConfigInvalid Class = "config_invalid"
	PolicyBlocked Class = "policy_blocked"
	AudioFailed   Class = "audio_failed"
	Unavailable   Class = "unavailable"
	AuthFailed    Class = "auth_failed"
	Timeout       Class = "timeout"
	EmptyResponse Class = "empty_response"
	ServerError   Class = "server_error"
	NoSpeech      Class = "no_speech"
	SinkFailed    Class = "sink_failed"
	Cancelled     Class = "cancelled"
	Internal      Class = "internal"
)

var Classes = []Class{
	ConfigInvalid, PolicyBlocked, AudioFailed, Unavailable, AuthFailed, Timeout,
	EmptyResponse, ServerError, NoSpeech, SinkFailed, Cancelled, Internal,
}

type Error struct {
	Class     Class
	Component string
	Msg       string
	Cause     error
}

func (e *Error) Error() string {
	if e.Component == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s: %s", e.Component, e.Msg)
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	if !ok {
		return false
	}
	return (other.Class == "" || e.Class == other.Class) &&
		(other.Component == "" || e.Component == other.Component)
}

func New(class Class, component, msg string) *Error {
	return &Error{Class: class, Component: component, Msg: msg}
}

func Wrap(class Class, component, msg string, cause error) *Error {
	return &Error{Class: class, Component: component, Msg: msg, Cause: cause}
}

func Classify(err error) Class {
	if err == nil {
		return ""
	}
	var effective interface{ EffectiveClass() Class }
	if errors.As(err, &effective) {
		return effective.EffectiveClass()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}
	if errors.Is(err, context.Canceled) {
		return Cancelled
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Class
	}
	return Internal
}
