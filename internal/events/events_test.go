package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestJournalMarshalFailureIsReportedAndNextEventIsWritten(t *testing.T) {
	bus := NewBus()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var failures []JournalOperation
	journal, err := NewJournal(bus, path, JournalOptions{
		MaxBytes: 1 << 20, MaxFiles: 2,
		OnFailure: func(operation JournalOperation) { failures = append(failures, operation) },
	})
	if err != nil {
		t.Fatal(err)
	}
	invalid := testEvent(StateChanged)
	invalid.Timestamp = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	bus.Publish(invalid)
	bus.Publish(testEvent(Terminal))
	closeErr := journal.Close()
	bus.Close()
	assertJournalFailure(t, closeErr, JournalMarshal)
	if len(failures) != 1 || failures[0] != JournalMarshal {
		t.Fatalf("failures=%v", failures)
	}
	if got := readJournalEventCount(t, path); got != 1 {
		t.Fatalf("written events=%d, want 1", got)
	}
}

func TestJournalStatFailureDoesNotDropCurrentOrLaterWrites(t *testing.T) {
	bus := NewBus()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	operations := defaultJournalOperations()
	stat := operations.stat
	calls := 0
	operations.stat = func(file *os.File) (os.FileInfo, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("stat temporaire")
		}
		return stat(file)
	}
	journal, err := NewJournal(bus, path, JournalOptions{
		MaxBytes: 1 << 20, MaxFiles: 2, operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	bus.Publish(testEvent(StateChanged))
	bus.Publish(testEvent(Terminal))
	closeErr := journal.Close()
	bus.Close()
	assertJournalFailure(t, closeErr, JournalStat)
	if got := readJournalEventCount(t, path); got != 2 {
		t.Fatalf("written events=%d, want 2", got)
	}
}

func TestJournalRenameFailureKeepsActiveFileAndRetriesRotation(t *testing.T) {
	bus := NewBus()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	operations := defaultJournalOperations()
	rename := operations.rename
	calls := 0
	operations.rename = func(source, destination string) error {
		calls++
		if calls == 1 {
			return errors.New("rename temporaire")
		}
		return rename(source, destination)
	}
	journal, err := NewJournal(bus, path, JournalOptions{
		MaxBytes: journalLineSize(t) + 1, MaxFiles: 2, operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		bus.Publish(testEvent(StateChanged))
	}
	closeErr := journal.Close()
	bus.Close()
	assertJournalFailure(t, closeErr, JournalRename)
	if calls < 2 {
		t.Fatalf("rename calls=%d, rotation was not retried", calls)
	}
	if got := readJournalEventCount(t, path); got != 3 {
		t.Fatalf("written events=%d, want 3", got)
	}
}

func TestJournalWriteFailureIsReportedAndLaterWriteSucceeds(t *testing.T) {
	bus := NewBus()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	operations := defaultJournalOperations()
	write := operations.write
	calls := 0
	operations.write = func(file *os.File, data []byte) (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("write temporaire")
		}
		return write(file, data)
	}
	journal, err := NewJournal(bus, path, JournalOptions{
		MaxBytes: 1 << 20, MaxFiles: 2, operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	bus.Publish(testEvent(StateChanged))
	bus.Publish(testEvent(Terminal))
	closeErr := journal.Close()
	bus.Close()
	assertJournalFailure(t, closeErr, JournalWrite)
	if got := readJournalEventCount(t, path); got != 1 {
		t.Fatalf("written events=%d, want 1", got)
	}
}

func TestJournalReopenFailureRestoresActiveFileAndRecovers(t *testing.T) {
	bus := NewBus()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	operations := defaultJournalOperations()
	open := operations.open
	calls := 0
	operations.open = func(path string, flags int) (*os.File, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("open temporaire")
		}
		return open(path, flags)
	}
	journal, err := NewJournal(bus, path, JournalOptions{
		MaxBytes: journalLineSize(t) + 1, MaxFiles: 2, operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		bus.Publish(testEvent(StateChanged))
	}
	closeErr := journal.Close()
	bus.Close()
	assertJournalFailure(t, closeErr, JournalReopen)
	if calls < 3 {
		t.Fatalf("open calls=%d, reopening was not retried", calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active journal missing after recovery: %v", err)
	}
	if got := readJournalEventCount(t, path); got != 3 {
		t.Fatalf("written events=%d, want 3", got)
	}
}

func TestJournalCloseReturnsFirstFailureAndSignalsOnlyOnce(t *testing.T) {
	bus := NewBus()
	secretPath := filepath.Join(t.TempDir(), "SECRET-session.jsonl")
	operations := defaultJournalOperations()
	operations.stat = func(*os.File) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "stat", Path: secretPath, Err: errors.New("refus")}
	}
	operations.write = func(*os.File, []byte) (int, error) {
		return 0, errors.New("write secondaire")
	}
	closeFile := operations.close
	operations.close = func(file *os.File) error {
		return errors.Join(closeFile(file), errors.New("close secondaire"))
	}
	var failures []JournalOperation
	journal, err := NewJournal(bus, secretPath, JournalOptions{
		MaxBytes: 1 << 20, MaxFiles: 2, operations: operations,
		OnFailure: func(operation JournalOperation) {
			failures = append(failures, operation)
			bus.Publish(testEvent(Degraded))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bus.Publish(testEvent(StateChanged))
	first := journal.Close()
	second := journal.Close()
	bus.Close()
	assertJournalFailure(t, first, JournalStat)
	assertJournalFailure(t, second, JournalStat)
	if len(failures) != 1 || failures[0] != JournalStat {
		t.Fatalf("failures=%v", failures)
	}
	if !strings.Contains(first.Error(), secretPath) {
		t.Fatalf("diagnostic error lost path: %v", first)
	}
	if message := JournalFailureMessage(failures[0]); strings.Contains(message, secretPath) || len(message) > 64 {
		t.Fatalf("unsafe or unbounded failure message: %q", message)
	}
}

func TestJournalCloseFailureIsReturned(t *testing.T) {
	bus := NewBus()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	operations := defaultJournalOperations()
	closeFile := operations.close
	operations.close = func(file *os.File) error {
		return errors.Join(closeFile(file), errors.New("close impossible"))
	}
	journal, err := NewJournal(bus, path, JournalOptions{
		MaxBytes: 1 << 20, MaxFiles: 2, operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	closeErr := journal.Close()
	bus.Close()
	assertJournalFailure(t, closeErr, JournalClose)
}

func assertJournalFailure(t *testing.T, err error, operation JournalOperation) {
	t.Helper()
	var failure *JournalError
	if !errors.As(err, &failure) || failure.Operation != operation {
		t.Fatalf("error=%v, want journal operation %s", err, operation)
	}
}

func journalLineSize(t *testing.T) int64 {
	t.Helper()
	line, err := json.Marshal(testEvent(StateChanged))
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(line) + 1)
}

func readJournalEventCount(t *testing.T, path string) int {
	t.Helper()
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("invalid journal line %q: %v", line, err)
			}
			count++
		}
	}
	return count
}
