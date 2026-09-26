package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
)

const (
	defaultTimeout      = 2 * time.Second
	maxRequestFrameSize = 4 * 1024
)

type Server struct {
	listener     net.Listener
	handler      Handler
	bus          *events.Bus
	readTimeout  atomic.Int64
	writeTimeout atomic.Int64
	lifecycle    context.Context
	cancel       context.CancelFunc
	closeOnce    sync.Once
	closeErr     error
	connections  map[net.Conn]struct{}
	connectionsM sync.Mutex
	connectionWG sync.WaitGroup
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
	lifecycle, cancel := context.WithCancel(context.Background())
	server := &Server{
		listener: listener, handler: handler, bus: bus,
		lifecycle: lifecycle, cancel: cancel, connections: make(map[net.Conn]struct{}),
	}
	server.SetTimeouts(defaultTimeout, defaultTimeout)
	return server, nil
}

func (s *Server) SetTimeouts(read, write time.Duration) {
	s.readTimeout.Store(int64(read))
	s.writeTimeout.Store(int64(write))
}

func (s *Server) Serve(ctx context.Context) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopLifecycleCancel := context.AfterFunc(s.lifecycle, cancel)
	defer stopLifecycleCancel()
	stopContextClose := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stopContextClose()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if serveCtx.Err() != nil || s.lifecycle.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.connectionWG.Wait()
				return nil
			}
			return errs.Wrap(errs.Unavailable, "ipc", "acceptation IPC impossible", err)
		}
		if serveCtx.Err() != nil || !s.trackConnection(conn) {
			_ = conn.Close()
			_ = s.Close()
			s.connectionWG.Wait()
			return nil
		}
		go func() {
			defer func() {
				_ = conn.Close()
				s.releaseConnection(conn)
			}()
			s.handleConnection(serveCtx, conn)
		}()
	}
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.closeErr = s.listener.Close()
		if errors.Is(s.closeErr, net.ErrClosed) {
			s.closeErr = nil
		}
		s.closeConnections()
	})
	return s.closeErr
}

func (s *Server) trackConnection(conn net.Conn) bool {
	s.connectionsM.Lock()
	defer s.connectionsM.Unlock()
	if s.lifecycle.Err() != nil {
		return false
	}
	s.connections[conn] = struct{}{}
	s.connectionWG.Add(1)
	return true
}

func (s *Server) releaseConnection(conn net.Conn) {
	s.connectionsM.Lock()
	delete(s.connections, conn)
	s.connectionsM.Unlock()
	s.connectionWG.Done()
}

func (s *Server) closeConnections() {
	s.connectionsM.Lock()
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.connectionsM.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(time.Duration(s.readTimeout.Load())))
	reader := bufio.NewReaderSize(conn, maxRequestFrameSize)
	request, err := readRequest(reader)
	if err != nil {
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
		s.serveSubscription(ctx, conn, reader)
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

func readRequest(reader *bufio.Reader) (Request, error) {
	frame, err := reader.ReadSlice('\n')
	if err != nil {
		return Request{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("plusieurs valeurs JSON dans la requête")
		}
		return Request{}, err
	}
	return request, nil
}

func (s *Server) serveSubscription(ctx context.Context, conn net.Conn, reader *bufio.Reader) {
	if s.bus == nil {
		s.writeResponse(conn, Failure(errs.New(errs.Unavailable, "ipc", "flux d'événements indisponible")))
		return
	}
	stream, unsubscribe := s.bus.Subscribe(32)
	defer unsubscribe()
	if err := s.writeResponse(conn, Success("subscribed")); err != nil {
		return
	}
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		var unexpected [1]byte
		_, _ = reader.Read(unexpected[:])
	}()
	defer func() {
		_ = conn.Close()
		<-clientGone
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
