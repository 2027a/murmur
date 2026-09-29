package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/pwrecord"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/ipc"
	"git.2027a.net/2027a/murmur/internal/stt"
	"git.2027a.net/2027a/murmur/internal/sysexec"
	"git.2027a.net/2027a/murmur/internal/textpipe"
)

// Use the production live recorder: FakeRecorder.Abort cleans even after Stop
// owns the capture, whereas pwrecord correctly leaves that decision to its caller.
func TestLiveStopFinalizesProductionCapture(t *testing.T) {
	for _, tc := range []struct {
		name        string
		textLimit   int
		maxSegments int
		cancel      bool
		keep        bool
		want        Result
		message     string
	}{
		{name: "text limit", textLimit: 2, want: Failed, message: stt.TextBudgetExceeded},
		{name: "segment limit", textLimit: 1024, maxSegments: 1, want: Failed, message: stt.SegmentLimitExceeded},
		{name: "success", textLimit: 1024, want: Completed},
		{name: "success with retention", textLimit: 1024, keep: true, want: Completed},
		{name: "cancel with retention", textLimit: 1024, keep: true, cancel: true, want: Cancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			capture := filepath.Join(root, "capture")
			if err := os.Mkdir(capture, 0o700); err != nil {
				t.Fatal(err)
			}
			process := sysexec.NewFakeProcess()
			signaled := make(chan struct{})
			var path string
			process.SignalFunc = func(signal os.Signal) error {
				if signal != os.Interrupt {
					return errors.New("unexpected capture signal")
				}
				// The initial PCM exposes exactly one segment. The tail is only
				// available after Stop owns finalization and has sent SIGINT.
				if err := wav.AppendPCM(path, make([]byte, 3*wav.ByteRate)); err != nil {
					return err
				}
				final, err := wav.FinalizeGrowing(path)
				if err != nil {
					return err
				}
				if err := final.Close(); err != nil {
					return err
				}
				process.Exit(nil)
				close(signaled)
				return nil
			}
			executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
				path = args[len(args)-1]
				if err := wav.CreateGrowing(path); err != nil {
					return nil, err
				}
				if err := wav.AppendPCM(path, make([]byte, 2*wav.ByteRate)); err != nil {
					return nil, err
				}
				return process, nil
			}}
			release := make(chan struct{})
			provider := &gatedLiveProvider{started: make(chan struct{}), release: release}
			chain, err := stt.NewChain(provider, nil, stt.MultiOptions{
				Workers: 1, MaxTextBytes: tc.textLimit, MaxSegments: tc.maxSegments,
			})
			if err != nil {
				t.Fatal(err)
			}
			target := &fakeSink{}
			processorStarted := make(chan struct{})
			diagnostics := filepath.Join(root, "diagnostics")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			o, err := NewOrchestrator(OrchestratorOptions{
				Context: ctx, SessionID: "live-cleanup", RuntimeDir: root,
				Recorder: pwrecord.New(executor), RecordOptions: audio.RecordOptions{
					Directory: capture, ReadinessInterval: time.Millisecond,
					Retention: audio.RetentionOptions{Keep: tc.keep, Directory: diagnostics},
				},
				ChunkDuration: time.Second, LiveChunking: true, Transcriber: chain,
				Processor: &fakeProcessor{started: processorStarted, result: textpipe.Result{Text: "final"}},
				Sink:      target,
			})
			if err != nil {
				t.Fatal(err)
			}
			done := runOrchestrator(o)
			await(t, provider.started)
			if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Stop)); !response.OK {
				t.Fatalf("stop response=%#v", response)
			}
			await(t, signaled)
			if tc.cancel {
				if response := o.Handle(context.Background(), ipc.NewRequest(ipc.Cancel)); !response.OK {
					t.Fatalf("cancel response=%#v", response)
				}
			}
			close(release)
			result := awaitResult(t, done)
			if result.Result != tc.want {
				t.Fatalf("result=%#v", result)
			}
			if tc.want == Failed && (result.Provider != "primary" || result.Error == nil || result.Error.Class != errs.ServerError || result.Error.Message != tc.message) {
				t.Fatalf("STT failure was masked: %#v", result)
			}
			if tc.want == Completed {
				if target.count() != 1 {
					t.Fatalf("sink calls=%d", target.count())
				}
			} else {
				if target.count() != 0 {
					t.Fatalf("sink called after failure/cancellation: %d", target.count())
				}
				select {
				case <-processorStarted:
					t.Fatal("processor called after failure/cancellation")
				default:
				}
			}
			entries, err := os.ReadDir(capture)
			if err != nil || len(entries) != 0 {
				t.Fatalf("capture not removed: entries=%v err=%v", entries, err)
			}
			if tc.keep && tc.want == Completed {
				entries, err := os.ReadDir(diagnostics)
				if err != nil || len(entries) != 1 {
					t.Fatalf("retained captures=%v err=%v", entries, err)
				}
			} else if _, err := os.Stat(diagnostics); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected diagnostic retention: %v", err)
			}
			// Cancellation may race the process waiter and request one kill
			// after Exit; it must not restart capture or signal it twice.
			if len(executor.Calls()) != 1 || len(process.Signals()) != 1 || process.KillCount() > 1 {
				t.Fatalf("capture lifecycle: calls=%v signals=%v kills=%d", executor.Calls(), process.Signals(), process.KillCount())
			}
		})
	}
}
