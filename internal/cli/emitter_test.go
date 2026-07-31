package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/events"
)

func TestCommandEmitterWriterFailureIsACommandFailure(t *testing.T) {
	const writerDetail = "unsafe writer detail"
	for _, test := range []struct {
		name        string
		writer      *failingEventWriter
		emitBefore  int
		wantEvents  int
		wantPartial bool
	}{
		{
			name:   "immediate",
			writer: &failingEventWriter{failAfter: 0, err: errors.New(writerDetail)},
		},
		{
			name:       "after N events",
			writer:     &failingEventWriter{failAfter: 2, err: errors.New(writerDetail)},
			emitBefore: 3,
			wantEvents: 2,
		},
		{
			name:        "partial write",
			writer:      &failingEventWriter{failAfter: 1, partial: 12},
			emitBefore:  2,
			wantEvents:  1,
			wantPartial: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			emitter := newCommandEmitter(true, test.writer, "cleanup")
			for range test.emitBefore {
				emitter.emit(events.StateChanged, "processing", "", "", "", 0, 0, "")
			}
			var stdout bytes.Buffer
			code := finishCommand(newCommandResult("cleanup"), nil, true, &stdout, test.writer, emitter, true)
			if code != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), test.writer.String())
			}
			result := decodeCommandResult(t, stdout.Bytes())
			if result.Success || result.Error == nil || result.Error.Class != errs.Internal ||
				result.Error.Component != "io" || result.Error.Message != "écriture du flux d'événements impossible" {
				t.Fatalf("result=%#v", result)
			}
			if strings.Contains(stdout.String(), writerDetail) || strings.Contains(test.writer.String(), writerDetail) ||
				strings.Contains(test.writer.String(), result.Error.Message) {
				t.Fatalf("writer detail or human error leaked: stdout=%q stderr=%q", stdout.String(), test.writer.String())
			}
			complete, partial := decodeEventPrefix(t, test.writer.Bytes())
			if complete != test.wantEvents || partial != test.wantPartial {
				t.Fatalf("complete events=%d partial=%v stderr=%q", complete, partial, test.writer.String())
			}
			if got, want := test.writer.Calls(), test.wantEvents+1; got != want {
				t.Fatalf("writer calls=%d, want %d", got, want)
			}
		})
	}
}

func TestCommandEmitterFailureDoesNotReplaceBusinessError(t *testing.T) {
	writer := &failingEventWriter{failAfter: 0, err: errors.New("unsafe writer detail")}
	emitter := newCommandEmitter(true, writer, "transcribe")
	runErr := errs.New(errs.AuthFailed, "stt/local", "authentification refusée")
	var stdout bytes.Buffer
	if code := finishCommand(newCommandResult("transcribe"), runErr, true, &stdout, writer, emitter, true); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), writer.String())
	}
	result := decodeCommandResult(t, stdout.Bytes())
	if result.Success || result.Error == nil || result.Error.Class != errs.AuthFailed ||
		result.Error.Component != "stt/local" || result.Error.Message != "authentification refusée" {
		t.Fatalf("result=%#v", result)
	}
	if writer.Len() != 0 || writer.Calls() != 1 {
		t.Fatalf("writer calls=%d stderr=%q", writer.Calls(), writer.String())
	}
}

func TestCommandEmitterFailureWithoutJSONUsesExitCodeOnly(t *testing.T) {
	writer := &failingEventWriter{failAfter: 0, err: errors.New("write failed")}
	emitter := newCommandEmitter(true, writer, "cleanup")
	emitter.emit(events.SessionStarted, "processing", "", "", "", 0, 0, "")
	result := newCommandResult("cleanup")
	result.TranscriptFinal = "must not become an error channel"
	var stdout bytes.Buffer
	if code := finishCommand(result, nil, false, &stdout, writer, emitter, true); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), writer.String())
	}
	if stdout.Len() != 0 || writer.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), writer.String())
	}
}

func TestCommandEmitterFailureIsConcurrencySafe(t *testing.T) {
	writer := &failingEventWriter{failAfter: 0, err: errors.New("write failed")}
	emitter := newCommandEmitter(true, writer, "transcribe")
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			emitter.emit(events.PartialStarted, "transcribing", "local", "", "stt", 1, 0, "")
			_ = emitter.failure()
		}()
	}
	group.Wait()
	if emitter.failure() == nil || writer.Calls() != 1 {
		t.Fatalf("failure=%v writer calls=%d", emitter.failure(), writer.Calls())
	}
}

func decodeCommandResult(t *testing.T, data []byte) commandResult {
	t.Helper()
	var result commandResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("invalid result JSON: %v: %q", err, data)
	}
	return result
}

func decodeEventPrefix(t *testing.T, data []byte) (int, bool) {
	t.Helper()
	complete := 0
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return complete, true
		}
		var event events.Event
		if err := json.Unmarshal(data[:newline], &event); err != nil {
			t.Fatalf("invalid complete JSONL event: %v: %q", err, data[:newline])
		}
		complete++
		data = data[newline+1:]
	}
	return complete, false
}

type failingEventWriter struct {
	mu        sync.Mutex
	output    bytes.Buffer
	failAfter int
	partial   int
	err       error
	calls     int
}

func (w *failingEventWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls <= w.failAfter {
		return w.output.Write(p)
	}
	if w.partial > 0 {
		n := min(w.partial, len(p))
		_, _ = w.output.Write(p[:n])
		return n, w.err
	}
	if w.err == nil {
		return 0, io.ErrShortWrite
	}
	return 0, w.err
}

func (w *failingEventWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.output.Bytes())
}

func (w *failingEventWriter) String() string {
	return string(w.Bytes())
}

func (w *failingEventWriter) Len() int {
	return len(w.Bytes())
}

func (w *failingEventWriter) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}
