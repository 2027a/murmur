package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
)

const defaultTimeout = 2 * time.Second

type Server struct {
	listener     net.Listener
	handler      Handler
	bus          *events.Bus
	readTimeout  atomic.Int64
	writeTimeout atomic.Int64
	connections  sync.WaitGroup
}

func Listen(path string, handler Handler, bus *events.Bus) (*Server, error) {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, errs.Wrap(errs.Unavailable, "ipc", "création du socket de session impossible", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, errs.Wrap(errs.Unavailable, "ipc", "permissions du socket de session impossibles", err)
	}
	server := &Server{listener: listener, handler: handler, bus: bus}
	server.SetTimeouts(defaultTimeout, defaultTimeout)
	return server, nil
}

func (s *Server) SetTimeouts(read, write time.Duration) {
	s.readTimeout.Store(int64(read))
	s.writeTimeout.Store(int64(write))
}

func (s *Server) Serve(ctx context.Context) error {
	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.listener.Close()
		case <-stopWatcher:
		}
	}()
	defer close(stopWatcher)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.connections.Wait()
				return nil
			}
			return errs.Wrap(errs.Unavailable, "ipc", "acceptation IPC impossible", err)
		}
		s.connections.Add(1)
		go func() {
			defer s.connections.Done()
			defer conn.Close()
			s.handleConnection(ctx, conn)
		}()
	}
}

func (s *Server) Close() error {
	return s.listener.Close()
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(time.Duration(s.readTimeout.Load())))
	decoder := json.NewDecoder(conn)
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		s.writeResponse(conn, Failure(errs.Wrap(errs.ConfigInvalid, "ipc", "requête JSON invalide", err)))
		return
	}
	if err := validateRequest(request); err != nil {
		s.writeResponse(conn, Failure(err))
		return
	}
	if request.Command == Subscribe {
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			s.writeResponse(conn, Failure(errs.Wrap(errs.Unavailable, "ipc", "suppression du délai de lecture impossible", err)))
			return
		}
		s.serveSubscription(ctx, conn)
		return
	}
	if s.handler == nil {
		s.writeResponse(conn, Failure(errs.New(errs.Unavailable, "ipc", "aucun gestionnaire de session disponible")))
		return
	}
	response := s.handler.Handle(ctx, request)
	response.Version = Version
	s.writeResponse(conn, response)
}

func (s *Server) serveSubscription(ctx context.Context, conn net.Conn) {
	if s.bus == nil {
		s.writeResponse(conn, Failure(errs.New(errs.Unavailable, "ipc", "flux d'événements indisponible")))
		return
	}
	stream, unsubscribe := s.bus.Subscribe(32)
	defer unsubscribe()
	if err := s.writeResponse(conn, Success("subscribed")); err != nil {
		return
	}
	clientGone := make(chan struct{}, 1)
	go func() {
		var unexpected [1]byte
		_, _ = conn.Read(unexpected[:])
		clientGone <- struct{}{}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-clientGone:
			return
		case event, ok := <-stream:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(time.Duration(s.writeTimeout.Load())))
			if err := events.EncodeJSONL(conn, event); err != nil {
				return
			}
		}
	}
}

func (s *Server) writeResponse(conn net.Conn, response Response) error {
	conn.SetWriteDeadline(time.Now().Add(time.Duration(s.writeTimeout.Load())))
	return json.NewEncoder(conn).Encode(response)
}
