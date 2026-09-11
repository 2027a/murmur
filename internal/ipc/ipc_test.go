package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestRequestFraming(t *testing.T) {
	tests := []struct {
		name      string
		frame     string
		wantValid bool
	}{
		{name: "one value", frame: "{\"v\":1,\"cmd\":\"status\"}\n", wantValid: true},
		{name: "whitespace after value", frame: "{\"v\":1,\"cmd\":\"status\"} \t\r\n", wantValid: true},
		{name: "second value", frame: "{\"v\":1,\"cmd\":\"status\"} {}\n"},
		{name: "non-whitespace suffix", frame: "{\"v\":1,\"cmd\":\"status\"} garbage\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var handlerCalls atomic.Int32
			_, path, stop := startServer(t, HandlerFunc(func(context.Context, Request) Response {
				handlerCalls.Add(1)
				return Success("handled")
			}), nil)
			defer stop()
			conn, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Write([]byte(test.frame)); err != nil {
				t.Fatal(err)
			}
			response := readRawResponse(t, conn)
			if test.wantValid {
				if !response.OK || response.State != "handled" {
					t.Fatalf("response = %#v", response)
				}
				if got := handlerCalls.Load(); got != 1 {
					t.Fatalf("handler calls = %d, want 1", got)
				}
				return
			}
			assertInvalidRequest(t, response)
			if got := handlerCalls.Load(); got != 0 {
				t.Fatalf("handler calls = %d, want 0", got)
			}
		})
	}
}

func TestOversizedRequestFrameRejected(t *testing.T) {
	var handlerCalls atomic.Int32
	_, path, stop := startServer(t, HandlerFunc(func(context.Context, Request) Response {
		handlerCalls.Add(1)
		return Success("handled")
	}), nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frame := "{\"v\":1,\"cmd\":\"status\"}" + strings.Repeat(" ", maxRequestFrameSize) + "\n"
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	assertInvalidRequest(t, readRawResponse(t, conn))
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

func TestRequestEOFBeforeLineFeedRejected(t *testing.T) {
	var handlerCalls atomic.Int32
	_, path, stop := startServer(t, HandlerFunc(func(context.Context, Request) Response {
		handlerCalls.Add(1)
		return Success("handled")
	}), nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{\"v\":1,\"cmd\":\"status\"}")); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	assertInvalidRequest(t, readRawResponse(t, conn))
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

func TestRequestWithoutLineFeedRejectedAfterDeadline(t *testing.T) {
	var handlerCalls atomic.Int32
	server, path, stop := startServer(t, HandlerFunc(func(context.Context, Request) Response {
		handlerCalls.Add(1)
		return Success("handled")
	}), nil)
	defer stop()
	server.SetTimeouts(50*time.Millisecond, time.Second)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{\"v\":1,\"cmd\":\"status\"}")); err != nil {
		t.Fatal(err)
	}
	assertInvalidRequest(t, readRawResponse(t, conn))
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

func TestConnectionHandlesOnlyFirstFrame(t *testing.T) {
	var handlerCalls atomic.Int32
	_, path, stop := startServer(t, HandlerFunc(func(context.Context, Request) Response {
		handlerCalls.Add(1)
		return Success("handled")
	}), nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frame := "{\"v\":1,\"cmd\":\"status\"}\n{\"v\":1,\"cmd\":\"stop\"}\n"
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(conn)
	var response Response
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.State != "handled" {
		t.Fatalf("response = %#v", response)
	}
	var second Response
	if err := decoder.Decode(&second); !errors.Is(err, io.EOF) {
		t.Fatalf("second response error = %v, want EOF", err)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

func TestSubscribeClosesOnBytesAfterRequestFrame(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	_, path, stop := startServer(t, nil, bus)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frame := "{\"v\":1,\"cmd\":\"subscribe\"}\n{\"v\":1,\"cmd\":\"status\"}\n"
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	response := readRawResponse(t, conn)
	if !response.OK || response.State != "subscribed" {
		t.Fatalf("response = %#v", response)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var event events.Event
	if err := json.NewDecoder(conn).Decode(&event); !errors.Is(err, io.EOF) {
		t.Fatalf("subscription read error = %v, want EOF", err)
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

func TestSubscribePreservesEventBufferedWithHandshake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	want := events.Event{
		Version:   1,
		Type:      events.StateChanged,
		Timestamp: time.Now().UTC(),
		SessionID: "buffered",
		State:     "recording",
	}
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		var request Request
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		response, err := json.Marshal(Success("subscribed"))
		if err != nil {
			serverDone <- err
			return
		}
		event, err := json.Marshal(want)
		if err != nil {
			serverDone <- err
			return
		}
		_, err = conn.Write(append(append(response, '\n'), append(event, '\n')...))
		serverDone <- err
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, errorStream, err := NewClient(path).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-stream:
		if got.SessionID != want.SessionID || got.State != want.State {
			t.Fatalf("event = %#v", got)
		}
	case err := <-errorStream:
		t.Fatalf("subscription error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("buffered event timeout")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
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

func TestServerCloseClosesActiveSubscriptions(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	path := filepath.Join(t.TempDir(), "session.sock")
	server, err := Listen(path, nil, bus)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()

	type subscription struct {
		stream <-chan events.Event
		errors <-chan error
	}
	subscriptions := make([]subscription, 3)
	for i := range subscriptions {
		stream, errorStream, err := NewClient(path).Subscribe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		subscriptions[i] = subscription{stream: stream, errors: errorStream}
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	for i, subscription := range subscriptions {
		select {
		case err := <-subscription.errors:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("subscription %d error = %v, want EOF", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscription %d did not observe EOF", i)
		}
		select {
		case _, ok := <-subscription.stream:
			if ok {
				t.Fatalf("subscription %d stream is still open", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscription %d stream did not close", i)
		}
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after Close")
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestServerCloseCancelsHandlerContext(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	path := filepath.Join(t.TempDir(), "session.sock")
	server, err := Listen(path, HandlerFunc(func(ctx context.Context, _ Request) Response {
		close(started)
		<-ctx.Done()
		close(canceled)
		return Success("canceled")
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()
	clientDone := make(chan error, 1)
	go func() {
		_, err := NewClient(path).Do(context.Background(), Status)
		clientDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("handler context was not canceled")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("client request did not return")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not wait for the canceled handler")
	}
}

func TestServerCloseConcurrentWithCancelAndAccept(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		path := filepath.Join(t.TempDir(), "session.sock")
		server, err := Listen(path, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(ctx) }()
		connection, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		closeErrors := make(chan error, 8)
		var goroutines sync.WaitGroup
		for i := 0; i < cap(closeErrors); i++ {
			goroutines.Add(1)
			go func() {
				defer goroutines.Done()
				<-start
				closeErrors <- server.Close()
			}()
		}
		for i := 0; i < 8; i++ {
			goroutines.Add(1)
			go func() {
				defer goroutines.Done()
				<-start
				conn, err := net.DialTimeout("unix", path, time.Second)
				if err == nil {
					_ = conn.Close()
				}
			}()
		}
		goroutines.Add(1)
		go func() {
			defer goroutines.Done()
			<-start
			cancel()
		}()
		close(start)
		goroutines.Wait()
		_ = connection.Close()
		close(closeErrors)
		for err := range closeErrors {
			if err != nil {
				t.Fatalf("iteration %d: Close: %v", iteration, err)
			}
		}
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("iteration %d: Serve: %v", iteration, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: Serve did not return", iteration)
		}
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

func readRawResponse(t *testing.T, conn net.Conn) Response {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func assertInvalidRequest(t *testing.T, response Response) {
	t.Helper()
	if response.Version != Version || response.OK || response.Error == nil || response.Error.Class != errs.ConfigInvalid || response.Error.Component != "ipc" || response.Error.Message != "requête JSON invalide" {
		t.Fatalf("response = %#v", response)
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
