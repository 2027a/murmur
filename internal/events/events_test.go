package events

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

func testEvent(kind Type) Event {
	return Event{
		Version:    1,
		Type:       kind,
		Timestamp:  time.Date(2026, 7, 29, 12, 34, 56, 0, time.UTC),
		SessionID:  "session-1",
		State:      "transcribing",
		Provider:   "speaches",
		ErrorClass: errs.Timeout,
	}
}

func TestEncodeJSONLGolden(t *testing.T) {
	var got bytes.Buffer
	if err := EncodeJSONL(&got, testEvent(PartialFailed)); err != nil {
		t.Fatal(err)
	}
	want := "{\"v\":1,\"type\":\"partial_failed\",\"timestamp\":\"2026-07-29T12:34:56Z\",\"session_id\":\"session-1\",\"state\":\"transcribing\",\"provider\":\"speaches\",\"error_class\":\"timeout\"}\n"
	if got.String() != want {
		t.Fatalf("JSONL:\n got %s want %s", got.String(), want)
	}
}

func TestEventRejectsUnknownType(t *testing.T) {
	event := testEvent("transcript")
	if err := event.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestBusSlowSubscriberDoesNotBlock(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	slow, unsubscribeSlow := bus.Subscribe(1)
	defer unsubscribeSlow()
	fast, unsubscribeFast := bus.Subscribe(128)
	defer unsubscribeFast()

	const count = 100
	done := make(chan struct{})
	go func() {
		for i := 0; i < count; i++ {
			bus.Publish(testEvent(StateChanged))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publisher blocked on slow subscriber")
	}
	if len(slow) != 1 {
		t.Fatalf("slow subscriber buffered %d events, want 1", len(slow))
	}
	if len(fast) == 0 {
		t.Fatal("fast subscriber received no event")
	}
}

func TestBusConcurrentPublishAndUnsubscribe(t *testing.T) {
	bus := NewBus()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		_, unsubscribe := bus.Subscribe(2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				bus.Publish(testEvent(StateChanged))
			}
		}()
		go func() {
			defer wg.Done()
			unsubscribe()
		}()
	}
	wg.Wait()
	bus.Close()
}

func TestSessionJournalRotatesAndContainsOnlyEvents(t *testing.T) {
	bus := NewBus()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	journal, err := NewJournal(bus, path, JournalOptions{MaxBytes: 512, MaxFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		bus.Publish(testEvent(StateChanged))
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	bus.Close()
	files, err := filepath.Glob(path + "*")
	if err != nil || len(files) == 0 || len(files) > 3 {
		t.Fatalf("journal files=%v error=%v", files, err)
	}
	for _, name := range files {
		info, err := os.Stat(name)
		if err != nil || info.Size() > 512 {
			t.Fatalf("journal %s size=%d error=%v", name, info.Size(), err)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var event Event
			if err := json.Unmarshal(line, &event); err != nil || event.Validate() != nil {
				t.Fatalf("invalid journal line %q: %v", line, err)
			}
		}
	}
}
