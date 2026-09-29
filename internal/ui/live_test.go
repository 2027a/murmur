package ui

import (
	"git.2027a.net/2027a/murmur/internal/events"
	"testing"
)

func TestLiveProgressDoesNotReplaceMicrophoneIndicator(t *testing.T) {
	for _, kind := range []events.Type{events.PartialStarted, events.PartialDone, events.PartialFailed} {
		for _, state := range []string{"recording", "stopping", "transcribing"} {
			event := events.Event{Type: kind, State: state, Partial: 1}
			_, notify := notificationForEvent(event)
			_, osd := OSDPayloadFromEvent(event)
			want := state == "transcribing"
			if notify != want || osd != want {
				t.Fatalf("%s %s: notify=%v osd=%v", kind, state, notify, osd)
			}
		}
	}
}
