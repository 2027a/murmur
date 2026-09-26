package ipc

import (
	"context"
	"errors"
	"fmt"

	"git.2027a.net/2027a/murmur/internal/errs"
)

const Version = 1

type Command string

const (
	Stop      Command = "stop"
	Cancel    Command = "cancel"
	Toggle    Command = "toggle"
	Status    Command = "status"
	Subscribe Command = "subscribe"
)

func (c Command) Valid() bool {
	return c == Stop || c == Cancel || c == Toggle || c == Status || c == Subscribe
}

type Request struct {
	Version int     `json:"v"`
	Command Command `json:"cmd"`
}

func NewRequest(command Command) Request {
	return Request{Version: Version, Command: command}
}

type Error struct {
	Class     errs.Class `json:"class"`
	Component string     `json:"component,omitempty"`
	Message   string     `json:"message"`
}

type Response struct {
	Version int    `json:"v"`
	OK      bool   `json:"ok"`
	State   string `json:"state,omitempty"`
	Result  string `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

func Success(state string) Response {
	return Response{Version: Version, OK: true, State: state}
}

func Failure(err error) Response {
	class := errs.Classify(err)
	component := "ipc"
	message := "erreur IPC"
	var typed *errs.Error
	if errors.As(err, &typed) {
		component = typed.Component
		message = typed.Msg
	}
	return Response{Version: Version, Error: &Error{Class: class, Component: component, Message: message}}
}

type Handler interface {
	Handle(context.Context, Request) Response
}

type HandlerFunc func(context.Context, Request) Response

func (f HandlerFunc) Handle(ctx context.Context, request Request) Response {
	return f(ctx, request)
}

func validateRequest(request Request) error {
	if request.Version != Version {
		return errs.New(errs.ConfigInvalid, "ipc", fmt.Sprintf("version IPC non supportée: %d", request.Version))
	}
	if !request.Command.Valid() {
		return errs.New(errs.ConfigInvalid, "ipc", fmt.Sprintf("commande IPC inconnue: %q", request.Command))
	}
	return nil
}
