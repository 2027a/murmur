package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/events"
)

type barFallback struct {
	mu        sync.Mutex
	seen      []events.Event
	dismissed int
}

func (f *barFallback) Show(e events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, e)
}
func (f *barFallback) Close()   {}
func (f *barFallback) Dismiss() { f.mu.Lock(); defer f.mu.Unlock(); f.dismissed++ }
func (f *barFallback) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen), f.dismissed
}

func waitBar(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("UI condition timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A real Unix bridge with a QML stand-in that can stop acknowledging frames.
func startBarBridge(t *testing.T, dir string) (*atomic.Bool, <-chan BarState, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	in, ack := io.Pipe()
	out, write := io.Pipe()
	ready := &atomic.Bool{}
	ready.Store(true)
	frames := make(chan BarState, 128)
	done := make(chan error, 1)
	go func() { done <- ServeBar(ctx, dir, in, write) }()
	go func() {
		decoder := json.NewDecoder(out)
		for {
			var state BarState
			if decoder.Decode(&state) != nil {
				return
			}
			select {
			case frames <- state:
			default:
			}
			if ready.Load() {
				_, _ = fmt.Fprintln(ack, state.Sequence)
			}
		}
	}()
	waitBar(t, func() bool { _, err := os.Stat(BarSocket(dir)); return err == nil })
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = ack.Close()
			_ = out.Close()
			_ = write.Close()
			_ = in.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(2 * time.Second):
				t.Error("bridge did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return ready, frames, stop
}

func TestBarRequiresQMLAckAndRecoversSnapshot(t *testing.T) {
	dir := t.TempDir()
	ack, frames, stop := startBarBridge(t, dir)
	if err := CheckBar(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	ack.Store(false)
	if err := CheckBar(context.Background(), dir); err == nil {
		t.Fatal("socket presence counted as functioning QML")
	}
	stop()
	fallback := &barFallback{}
	b := NewBar(BarSocket(dir), "clipboard", true, fallback, nil)
	defer b.Close()
	event := events.Event{Version: 1, Type: events.CaptureReady, Timestamp: time.Now(), SessionID: "test", State: "recording"}
	b.Show(event)
	waitBar(t, func() bool { n, _ := fallback.counts(); return n > 0 })
	ack, frames, _ = startBarBridge(t, dir)
	waitBar(t, func() bool { _, n := fallback.counts(); return n > 0 })
	b.Meter(wav.Level{RMS: .5, Peak: .75, UpdatedMS: time.Now().UnixMilli()})
	var recovered BarState
	waitBar(t, func() bool {
		select {
		case recovered = <-frames:
			return recovered.Level.RMS == .5
		default:
			return false
		}
	})
	if recovered.State != "recording" || recovered.CaptureMS == 0 || recovered.Delivery != "clipboard" {
		t.Fatalf("snapshot=%+v", recovered)
	}
	// Lose the renderer during a long processing step, without any new event.
	event.Type = events.StateChanged
	event.State = "processing"
	b.Show(event)
	waitBar(t, func() bool {
		select {
		case recovered = <-frames:
			return recovered.State == "processing"
		default:
			return false
		}
	})
	notifications, _ := fallback.counts()
	ack.Store(false)
	waitBar(t, func() bool { n, _ := fallback.counts(); return n > notifications })
	ack.Store(true)
	waitBar(t, func() bool { _, n := fallback.counts(); return n > 1 })
	// Background STT does not steal recording; a late level cannot undo stop.
	event.Type = events.PartialDone
	b.Show(event)
	event.Type = events.StateChanged
	event.State = "processing"
	b.Show(event)
	b.Meter(wav.Level{RMS: 1, Peak: 1})
	event.Type = events.Terminal
	event.State = "idle"
	event.Result = "completed"
	b.Show(event)
	b.Close()
	waitBar(t, func() bool {
		select {
		case recovered = <-frames:
			return recovered.Result == "completed"
		default:
			return false
		}
	})
	if recovered.Level.RMS != 0 || recovered.FinishedMS == 0 {
		t.Fatalf("terminal=%+v", recovered)
	}
}

func TestBarSlowFrontendNeverBlocksCaptureOrLosesTerminal(t *testing.T) {
	dir := t.TempDir()
	ack, _, _ := startBarBridge(t, dir)
	ack.Store(false)
	fallback := &barFallback{}
	b := NewBar(BarSocket(dir), "paste", true, fallback, nil)
	e := events.Event{Version: 1, Type: events.CaptureReady, SessionID: "slow", State: "recording", Timestamp: time.Now()}
	b.Show(e)
	start := time.Now()
	for range 10000 {
		b.Meter(wav.Level{RMS: .1, UpdatedMS: time.Now().UnixMilli()})
	}
	e.Type = events.Terminal
	e.State = "idle"
	e.Result = "cancelled"
	b.Show(e)
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("capture blocked by UI")
	}
	b.Close()
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	if len(fallback.seen) == 0 || fallback.seen[len(fallback.seen)-1].Result != "cancelled" {
		t.Fatalf("terminal lost: %+v", fallback.seen)
	}
}

func TestForcedBarReportsUnavailableWithoutNotifications(t *testing.T) {
	var warnings atomic.Int32
	b := NewBar(BarSocket(t.TempDir()), "paste", false, nil, func(string, ...any) { warnings.Add(1) })
	b.Show(events.Event{Version: 1, Type: events.SessionStarted, SessionID: "forced", State: "starting", Timestamp: time.Now()})
	b.Close()
	if warnings.Load() != 1 {
		t.Fatalf("warnings=%d", warnings.Load())
	}
}

func TestBarBridgeReleasesLockWhenStdoutIsBlocked(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin, input := io.Pipe()
	stdout, output := io.Pipe() // Deliberately never drain stdout.
	defer stdin.Close()
	defer input.Close()
	defer stdout.Close()
	defer output.Close()
	done := make(chan error, 1)
	go func() { done <- ServeBar(ctx, dir, stdin, output) }()
	waitBar(t, func() bool { _, err := os.Stat(BarSocket(dir)); return err == nil })
	if err := CheckBar(context.Background(), dir); err == nil {
		t.Fatal("undrained renderer accepted")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked output not reported")
		}
	case <-time.After(time.Second):
		t.Fatal("bridge held lock after output deadline")
	}
	// A fresh shell can acquire the lock even if the old reader never resumed.
	_, _, _ = startBarBridge(t, dir)
	if err := CheckBar(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
}
