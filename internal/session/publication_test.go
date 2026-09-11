package session

import (
	"sync"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/events"
)

type collectingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *collectingPublisher) Publish(event events.Event) {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
}

func TestPublicationGateSealsAfterSingleTerminal(t *testing.T) {
	target := &collectingPublisher{}
	gate := newPublicationGate(target)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate.Publish(events.Event{Type: events.StateChanged})
		}()
	}
	gate.Publish(events.Event{Type: events.Terminal, Result: "cancelled"})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate.Publish(events.Event{Type: events.PartialDone})
			gate.Publish(events.Event{Type: events.Terminal, Result: "failed"})
		}()
	}
	wg.Wait()

	target.mu.Lock()
	defer target.mu.Unlock()
	terminals := 0
	seenTerminal := false
	for _, event := range target.events {
		if seenTerminal {
			t.Fatalf("event after terminal: %#v", event)
		}
		if event.Type == events.Terminal {
			seenTerminal = true
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events=%d", terminals)
	}
}
