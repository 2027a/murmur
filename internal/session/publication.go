package session

import (
	"sync"

	"git.2027a.net/mathieu/murmur/internal/events"
)

// publicationGate serializes one session's stream and seals it after terminal.
type publicationGate struct {
	mu     sync.Mutex
	target publisher
	sealed bool
}

func newPublicationGate(target publisher) *publicationGate {
	return &publicationGate{target: target}
}

func (g *publicationGate) Publish(event events.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return
	}
	if g.target != nil {
		g.target.Publish(event)
	}
	if event.Type == events.Terminal {
		g.sealed = true
	}
}

func (g *publicationGate) setTarget(target publisher) {
	g.mu.Lock()
	g.target = target
	g.mu.Unlock()
}
