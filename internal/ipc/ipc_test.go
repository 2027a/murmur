package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
)

func TestCommandRoundTrips(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	server, path, stop := startServer(t, HandlerFunc(func(_ context.Context, request Request) Response {
		return Success(string(request.Command))
	}), bus)
	defer stop()
	_ = server
	client := NewClient(path)
	for _, command := range []Command{Stop, Cancel, Toggle, Status} {
		t.Run(string(command), func(t *testing.T) {
			response, err := client.Do(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			if response.State != string(command) {
				t.Fatalf("state = %q", response.State)
			}
		})
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions = %o", info.Mode().Perm())
	}
}

func TestUnknownVersionRejected(t *testing.T) {
	_, path, stop := startServer(t, HandlerFunc(func(context.Context, Request) Response {
		t.Fatal("handler must not run")
		return Response{}
	}), nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{\"v\":99,\"cmd\":\"status\"}\n")); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Class != errs.ConfigInvalid {
		t.Fatalf("response = %#v", response)
	}
}

func TestAbsentSocketIsNoSession(t *testing.T) {
	client := NewClient(filepath.Join(t.TempDir(), "missing.sock"))
	_, err := client.Do(context.Background(), Status)
	if !errors.Is(err, ErrNoSession) || errs.Classify(err) != errs.Unavailable {
		t.Fatalf("error = %v", err)
	}
}

func TestSubscribeStreamsEvents(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	_, path, stop := startServer(t, nil, bus)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, errors, err := NewClient(path).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := events.Event{
		Version:   1,
		Type:      events.StateChanged,
		Timestamp: time.Now().UTC(),
		SessionID: "session-1",
		State:     "recording",
	}
	bus.Publish(want)
	select {
	case got := <-stream:
		if got.Type != want.Type || got.SessionID != want.SessionID || got.State != want.State {
			t.Fatalf("event = %#v", got)
		}
	case err := <-errors:
		t.Fatalf("subscription error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("event timeout")
	}
}

func TestSubscribeSurvivesServerReadTimeout(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	server, path, stop := startServer(t, nil, bus)
	defer stop()
	server.SetTimeouts(100*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, errors, err := NewClient(path).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	bus.Publish(events.Event{Version: 1, Type: events.StateChanged, Timestamp: time.Now().UTC(), SessionID: "late", State: "recording"})
	select {
	case event := <-stream:
		if event.SessionID != "late" {
			t.Fatalf("event = %#v", event)
		}
	case err := <-errors:
		t.Fatalf("subscription error after read timeout: %v", err)
	case <-time.After(time.Second):
		t.Fatal("late event timeout")
	}
}

func TestClientReadTimeout(t *testing.T) {
	_, path, stop := startServer(t, HandlerFunc(func(context.Context, Request) Response {
		time.Sleep(100 * time.Millisecond)
		return Success("late")
	}), nil)
	defer stop()
	client := NewClient(path)
	client.ReadTimeout = 10 * time.Millisecond
	_, err := client.Do(context.Background(), Status)
	if errs.Classify(err) != errs.Unavailable {
		t.Fatalf("error = %v", err)
	}
}

func startServer(t *testing.T, handler Handler, bus *events.Bus) (*Server, string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.sock")
	server, err := Listen(path, handler, bus)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	return server, path, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("server did not stop")
		}
	}
}
