package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
)

var ErrNoSession = errs.New(errs.Unavailable, "ipc-no-session", "aucune session active")

type Client struct {
	Path         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func NewClient(path string) *Client {
	return &Client{Path: path, ReadTimeout: defaultTimeout, WriteTimeout: defaultTimeout}
}

func (c *Client) Do(ctx context.Context, command Command) (Response, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	stopContextClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopContextClose()
	if err := c.writeRequest(conn, NewRequest(command)); err != nil {
		return Response{}, err
	}
	response, err := c.readResponse(conn)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	if !response.OK {
		return response, responseError(response)
	}
	return response, nil
}

func (c *Client) Subscribe(ctx context.Context) (<-chan events.Event, <-chan error, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	stopContextClose := context.AfterFunc(ctx, func() { conn.Close() })
	if err := c.writeRequest(conn, NewRequest(Subscribe)); err != nil {
		stopContextClose()
		conn.Close()
		return nil, nil, err
	}
	decoder := json.NewDecoder(conn)
	response, err := c.readResponseWithDecoder(conn, decoder)
	if err != nil {
		stopContextClose()
		conn.Close()
		return nil, nil, err
	}
	if !response.OK {
		stopContextClose()
		conn.Close()
		return nil, nil, responseError(response)
	}
	conn.SetReadDeadline(time.Time{})
	eventStream := make(chan events.Event, 16)
	errorStream := make(chan error, 1)
	go func() {
		defer close(eventStream)
		defer close(errorStream)
		defer conn.Close()
		defer stopContextClose()
		for {
			var event events.Event
			if err := decoder.Decode(&event); err != nil {
				if ctx.Err() == nil {
					errorStream <- err
				}
				return
			}
			if err := event.Validate(); err != nil {
				errorStream <- err
				return
			}
			select {
			case eventStream <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return eventStream, errorStream, nil
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNoSession
		}
		return nil, errs.Wrap(errs.Unavailable, "ipc", "connexion à la session impossible", err)
	}
	return conn, nil
}

func (c *Client) writeRequest(conn net.Conn, request Request) error {
	conn.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return errs.Wrap(errs.Unavailable, "ipc", "écriture de la requête impossible", err)
	}
	return nil
}

func (c *Client) readResponse(conn net.Conn) (Response, error) {
	return c.readResponseWithDecoder(conn, json.NewDecoder(conn))
}

func (c *Client) readResponseWithDecoder(conn net.Conn, decoder *json.Decoder) (Response, error) {
	conn.SetReadDeadline(time.Now().Add(c.ReadTimeout))
	var response Response
	if err := decoder.Decode(&response); err != nil {
		return Response{}, errs.Wrap(errs.Unavailable, "ipc", "lecture de la réponse impossible", err)
	}
	if response.Version != Version {
		return Response{}, errs.New(errs.ConfigInvalid, "ipc", "version de réponse IPC non supportée")
	}
	return response, nil
}

func responseError(response Response) error {
	if response.Error == nil {
		return errs.New(errs.Internal, "ipc", "réponse IPC en échec sans détail")
	}
	return errs.New(response.Error.Class, response.Error.Component, response.Error.Message)
}
