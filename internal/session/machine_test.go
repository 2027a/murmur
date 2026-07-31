package session

import (
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
)

func TestMachineNominalLifecycle(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	stream, unsubscribe := bus.Subscribe(16)
	defer unsubscribe()
	machine := NewMachine("s1", bus)
	machine.now = func() time.Time { return time.Unix(1, 0) }

	commands := []Command{
		StartCommand, CaptureReadyCommand, StopCommand, CaptureStoppedCommand,
		TranscriptionDoneCommand, ProcessingDoneCommand, DeliveryDoneCommand,
	}
	for _, command := range commands {
		if err := machine.Apply(command); err != nil {
			t.Fatalf("Apply(%s): %v", command, err)
		}
	}
	if got := machine.Result(); got != Completed {
		t.Fatalf("result = %s, want completed", got)
	}
	if got := len(stream); got != len(commands) {
		t.Fatalf("events = %d, want one per transition (%d)", got, len(commands))
	}
}

func TestStopDuringStartingIsDeferred(t *testing.T) {
	machine := NewMachine("s1", nil)
	if err := machine.Apply(StartCommand); err != nil {
		t.Fatal(err)
	}
	if err := machine.Apply(StopCommand); err != nil {
		t.Fatal(err)
	}
	if machine.State() != Starting || !machine.StopPending() {
		t.Fatalf("state=%s pending=%v", machine.State(), machine.StopPending())
	}
	if err := machine.Apply(CaptureReadyCommand); err != nil {
		t.Fatal(err)
	}
	if machine.State() != Stopping {
		t.Fatalf("state = %s, want stopping", machine.State())
	}
}

func TestCancelNeverBecomesFailed(t *testing.T) {
	machine := machineAt(Transcribing)
	if err := machine.Apply(CancelCommand); err != nil {
		t.Fatal(err)
	}
	if err := machine.Apply(FailCommand); errs.Classify(err) != errs.Internal {
		t.Fatalf("post-terminal fail error = %v", err)
	}
	if machine.Result() != Cancelled {
		t.Fatalf("result = %s, want cancelled", machine.Result())
	}
}

func TestCancelDuringDeliveringIsNonDestructive(t *testing.T) {
	machine := machineAt(Delivering)
	if err := machine.Apply(CancelCommand); errs.Classify(err) != errs.Unavailable {
		t.Fatalf("cancel error = %v", err)
	}
	if machine.State() != Delivering || machine.Result() != "" {
		t.Fatalf("state=%s result=%s", machine.State(), machine.Result())
	}
	if err := machine.Apply(DeliveryDoneCommand); err != nil {
		t.Fatal(err)
	}
	if machine.Result() != Completed {
		t.Fatalf("result = %s", machine.Result())
	}
}

func TestStateCommandMatrix(t *testing.T) {
	allowed := map[State]map[Command]bool{
		Idle:         {StartCommand: true, ToggleCommand: true},
		Starting:     {CaptureReadyCommand: true, StopCommand: true, ToggleCommand: true, CancelCommand: true, FailCommand: true},
		Recording:    {StopCommand: true, ToggleCommand: true, CancelCommand: true, FailCommand: true},
		Stopping:     {CaptureStoppedCommand: true, NoSpeechCommand: true, CancelCommand: true, FailCommand: true},
		Transcribing: {TranscriptionDoneCommand: true, NoSpeechCommand: true, CancelCommand: true, FailCommand: true},
		Processing:   {ProcessingDoneCommand: true, NoSpeechCommand: true, CancelCommand: true, FailCommand: true},
		Delivering:   {DeliveryDoneCommand: true, FailCommand: true},
	}
	for _, state := range States {
		for _, command := range Commands {
			t.Run(string(state)+"/"+string(command), func(t *testing.T) {
				machine := machineAt(state)
				err := machine.Apply(command)
				if allowed[state][command] && err != nil {
					t.Fatalf("allowed transition rejected: %v", err)
				}
				if !allowed[state][command] && err == nil {
					t.Fatal("illegal transition accepted")
				}
				if err != nil && errs.Classify(err) == "" {
					t.Fatalf("unclassified error: %v", err)
				}
			})
		}
	}
}

func machineAt(state State) *Machine {
	machine := NewMachine("test", nil)
	machine.state = state
	return machine
}
