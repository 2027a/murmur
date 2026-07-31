package session

import (
	"fmt"
	"sync"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
)

type State string

const (
	Idle         State = "idle"
	Starting     State = "starting"
	Recording    State = "recording"
	Stopping     State = "stopping"
	Transcribing State = "transcribing"
	Processing   State = "processing"
	Delivering   State = "delivering"
)

var States = []State{Idle, Starting, Recording, Stopping, Transcribing, Processing, Delivering}

type Result string

const (
	Completed Result = "completed"
	Cancelled Result = "cancelled"
	NoSpeech  Result = "no_speech"
	Failed    Result = "failed"
)

type Command string

const (
	StartCommand             Command = "start"
	StopCommand              Command = "stop"
	CancelCommand            Command = "cancel"
	ToggleCommand            Command = "toggle"
	CaptureReadyCommand      Command = "capture_ready"
	CaptureStoppedCommand    Command = "capture_stopped"
	TranscriptionDoneCommand Command = "transcription_done"
	ProcessingDoneCommand    Command = "processing_done"
	NoSpeechCommand          Command = "no_speech"
	DeliveryDoneCommand      Command = "delivery_done"
	FailCommand              Command = "fail"
)

var Commands = []Command{
	StartCommand, StopCommand, CancelCommand, ToggleCommand, CaptureReadyCommand,
	CaptureStoppedCommand, TranscriptionDoneCommand, ProcessingDoneCommand,
	NoSpeechCommand, DeliveryDoneCommand, FailCommand,
}

type publisher interface {
	Publish(events.Event)
}

type Machine struct {
	mu          sync.Mutex
	sessionID   string
	state       State
	result      Result
	stopPending bool
	bus         publisher
	now         func() time.Time
}

func NewMachine(sessionID string, bus publisher) *Machine {
	return &Machine{sessionID: sessionID, state: Idle, bus: bus, now: time.Now}
}

func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *Machine) Result() Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.result
}

func (m *Machine) StopPending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopPending
}

func (m *Machine) Apply(command Command) error {
	return m.apply(command, "", "")
}

func (m *Machine) ApplyTerminal(command Command, provider string, class errs.Class) error {
	return m.apply(command, provider, class)
}

func (m *Machine) SetPublisher(bus publisher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bus = bus
}

func (m *Machine) apply(command Command, provider string, class errs.Class) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.result != "" {
		return m.illegal(command)
	}

	switch m.state {
	case Idle:
		if command == StartCommand || command == ToggleCommand {
			m.transition(Starting, events.SessionStarted)
			return nil
		}
	case Starting:
		switch command {
		case CaptureReadyCommand:
			if m.stopPending {
				m.transition(Stopping, events.StateChanged)
			} else {
				m.transition(Recording, events.CaptureReady)
			}
			return nil
		case StopCommand, ToggleCommand:
			m.stopPending = true
			return nil
		case CancelCommand:
			m.terminal(Cancelled, provider, class)
			return nil
		case FailCommand:
			m.terminal(Failed, provider, class)
			return nil
		}
	case Recording:
		switch command {
		case StopCommand, ToggleCommand:
			m.transition(Stopping, events.StateChanged)
			return nil
		case CancelCommand:
			m.terminal(Cancelled, provider, class)
			return nil
		case FailCommand:
			m.terminal(Failed, provider, class)
			return nil
		}
	case Stopping:
		switch command {
		case CaptureStoppedCommand:
			m.transition(Transcribing, events.StateChanged)
			return nil
		case NoSpeechCommand:
			m.terminal(NoSpeech, provider, class)
			return nil
		case CancelCommand:
			m.terminal(Cancelled, provider, class)
			return nil
		case FailCommand:
			m.terminal(Failed, provider, class)
			return nil
		}
	case Transcribing:
		switch command {
		case TranscriptionDoneCommand:
			m.transition(Processing, events.StateChanged)
			return nil
		case CancelCommand:
			m.terminal(Cancelled, provider, class)
			return nil
		case NoSpeechCommand:
			m.terminal(NoSpeech, provider, class)
			return nil
		case FailCommand:
			m.terminal(Failed, provider, class)
			return nil
		}
	case Processing:
		switch command {
		case ProcessingDoneCommand:
			m.transition(Delivering, events.StateChanged)
			return nil
		case NoSpeechCommand:
			m.terminal(NoSpeech, provider, class)
			return nil
		case CancelCommand:
			m.terminal(Cancelled, provider, class)
			return nil
		case FailCommand:
			m.terminal(Failed, provider, class)
			return nil
		}
	case Delivering:
		switch command {
		case DeliveryDoneCommand:
			m.terminal(Completed, provider, class)
			return nil
		case FailCommand:
			m.terminal(Failed, provider, class)
			return nil
		case CancelCommand:
			return errs.New(errs.Unavailable, "session", "annulation refusée: la livraison a déjà commencé")
		}
	}

	if command == ToggleCommand && (m.state == Stopping || m.state == Transcribing || m.state == Processing || m.state == Delivering) {
		return errs.New(errs.Unavailable, "session", fmt.Sprintf("toggle refusé pendant l'état %s", m.state))
	}
	return m.illegal(command)
}

func (m *Machine) transition(state State, kind events.Type) {
	m.state = state
	m.publish(events.Event{
		Version:   1,
		Type:      kind,
		Timestamp: m.now().UTC(),
		SessionID: m.sessionID,
		State:     string(state),
	})
}

func (m *Machine) terminal(result Result, provider string, errorClass errs.Class) {
	m.result = result
	m.state = Idle
	if errorClass == "" {
		if result == Cancelled {
			errorClass = errs.Cancelled
		} else if result == NoSpeech {
			errorClass = errs.NoSpeech
		} else if result == Failed {
			errorClass = errs.Internal
		}
	}
	m.publish(events.Event{
		Version:    1,
		Type:       events.Terminal,
		Timestamp:  m.now().UTC(),
		SessionID:  m.sessionID,
		State:      string(Idle),
		Result:     string(result),
		Provider:   provider,
		ErrorClass: errorClass,
	})
}

func (m *Machine) publish(event events.Event) {
	if m.bus != nil {
		m.bus.Publish(event)
	}
}

func (m *Machine) illegal(command Command) error {
	return errs.New(errs.Internal, "session", fmt.Sprintf("transition illégale: état=%s commande=%s", m.state, command))
}
