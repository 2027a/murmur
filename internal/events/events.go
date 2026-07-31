package events

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

type Type string

const (
	SessionStarted   Type = "session_started"
	CaptureReady     Type = "capture_ready"
	StateChanged     Type = "state_changed"
	PartialStarted   Type = "partial_started"
	PartialDone      Type = "partial_done"
	PartialFailed    Type = "partial_failed"
	ProviderSelected Type = "provider_selected"
	Degraded         Type = "degraded"
	Delivered        Type = "delivered"
	Terminal         Type = "terminal"
)

var validTypes = map[Type]struct{}{
	SessionStarted: {}, CaptureReady: {}, StateChanged: {}, PartialStarted: {},
	PartialDone: {}, PartialFailed: {}, ProviderSelected: {}, Degraded: {},
	Delivered: {}, Terminal: {},
}

// Event intentionally has no transcript or arbitrary message field.
type Event struct {
	Version    int        `json:"v"`
	Type       Type       `json:"type"`
	Timestamp  time.Time  `json:"timestamp"`
	SessionID  string     `json:"session_id"`
	State      string     `json:"state,omitempty"`
	Result     string     `json:"result,omitempty"`
	Provider   string     `json:"provider,omitempty"`
	ErrorClass errs.Class `json:"error_class,omitempty"`
	Component  string     `json:"component,omitempty"`
	Partial    int        `json:"partial,omitempty"`
	Total      int        `json:"total,omitempty"`
}

func (e Event) Validate() error {
	if e.Version != 1 {
		return fmt.Errorf("version d'événement invalide: %d", e.Version)
	}
	if _, ok := validTypes[e.Type]; !ok {
		return fmt.Errorf("type d'événement invalide: %q", e.Type)
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("horodatage d'événement manquant")
	}
	if e.SessionID == "" {
		return fmt.Errorf("identifiant de session manquant")
	}
	return nil
}

func EncodeJSONL(w io.Writer, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(event)
}

type Bus struct {
	mu          sync.RWMutex
	subscribers map[uint64]chan Event
	nextID      uint64
	closed      bool
}

func NewBus() *Bus {
	return &Bus{subscribers: make(map[uint64]chan Event)}
}

// Subscribe returns a bounded stream. Events are dropped for a slow subscriber.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Event, buffer)
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subscribers[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if subscriber, ok := b.subscribers[id]; ok {
				delete(b.subscribers, id)
				close(subscriber)
			}
		})
	}
}

func (b *Bus) Publish(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, subscriber := range b.subscribers {
		close(subscriber)
		delete(b.subscribers, id)
	}
}
