package pwrecord

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/stt"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func TestStartArgumentsReadinessAndStop(t *testing.T) {
	root := t.TempDir()
	var path string
	process := sysexec.NewFakeProcess()
	process.SignalFunc = func(signal os.Signal) error {
		if signal != os.Interrupt {
			t.Errorf("Signal() = %v", signal)
		}
		writeWAV(t, path, 200*time.Millisecond)
		process.Exit(nil)
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, name string, args []string) (sysexec.Process, error) {
		if name != "pw-record" {
			t.Fatalf("commande = %q", name)
		}
		path = args[len(args)-1]
		writeWAV(t, path, 0)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory:         root,
		Target:            "alsa_input.test",
		ReadinessTimeout:  time.Second,
		ReadinessInterval: 5 * time.Millisecond,
		MinimumDuration:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"--media-category", "Capture",
		"--media-role", "Communication",
		"--rate", "16000",
		"--channels", "1",
		"--format", "s16",
		"--target", "alsa_input.test",
		path,
	}
	if calls := executor.Calls(); len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, wantArgs) {
		t.Fatalf("appel = %#v", calls)
	}
	select {
	case err := <-recording.Ready():
		t.Fatalf("readiness annoncée sur un simple en-tête: %v", err)
	default:
	}
	appendSample(t, path)
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	file, err := recording.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	awaitDone(t, recording.Done())
	if err := recording.Err(); err != nil {
		t.Fatalf("Err() après stop = %v", err)
	}
	if file.Duration() != 200*time.Millisecond {
		t.Fatalf("Duration() = %v", file.Duration())
	}
	if len(process.Signals()) != 1 || process.Signals()[0] != os.Interrupt {
		t.Fatalf("signaux = %v", process.Signals())
	}
	if _, err := file.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStopAcceptsNonZeroExitWhenWAVWasFinalized(t *testing.T) {
	root := t.TempDir()
	var path string
	process := sysexec.NewFakeProcess()
	process.SignalFunc = func(os.Signal) error {
		writeWAV(t, path, 200*time.Millisecond)
		process.Exit(errors.New("pw-record exits 1 after SIGINT"))
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		writeWAV(t, path, 200*time.Millisecond)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	file, err := recording.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if file.Duration() != 200*time.Millisecond {
		t.Fatalf("Duration() = %v", file.Duration())
	}
	_, _ = file.Release()
}

func TestStartWithoutTarget(t *testing.T) {
	process := sysexec.NewFakeProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		for _, arg := range args {
			if arg == "--target" {
				t.Fatal("--target présent sans cible configurée")
			}
		}
		process.Exit(errors.New("stopped"))
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("Ready() = %v", err)
	}
}

func TestReadinessFailsWhenProcessDiesAndCleansUp(t *testing.T) {
	root := t.TempDir()
	process := sysexec.NewFakeProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		writeWAV(t, args[len(args)-1], 0)
		process.Exit(errors.New("boom"))
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{Directory: root, ReadinessInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("Ready() = %v", err)
	}
	waitEmpty(t, root)
}

func TestDoneReportsProcessDeathAfterReadiness(t *testing.T) {
	root := t.TempDir()
	process := sysexec.NewFakeProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		writeWAV(t, args[len(args)-1], 200*time.Millisecond)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	process.Exit(errors.New("boom"))
	awaitDone(t, recording.Done())
	if err := recording.Err(); errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("Err() = %v", err)
	}
	_ = recording.Abort()
	waitEmpty(t, root)
}

func TestStopAfterObservedProcessExitAlwaysFails(t *testing.T) {
	for _, test := range []struct {
		name    string
		exitErr error
	}{
		{name: "exit zero"},
		{name: "exit nonzero", exitErr: errors.New("pw-record exits 1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			process := sysexec.NewFakeProcess()
			executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
				writeWAV(t, args[len(args)-1], 200*time.Millisecond)
				return process, nil
			}}
			recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
				Directory: root, ReadinessInterval: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := receiveReady(t, recording.Ready()); err != nil {
				t.Fatal(err)
			}

			process.Exit(test.exitErr)
			awaitDone(t, recording.Done())
			file, err := recording.Stop(context.Background())
			if errs.Classify(err) != errs.AudioFailed {
				t.Fatalf("Stop() error=%v", err)
			}
			if file.Path() != "" {
				t.Fatalf("Stop() a exposé %q après une sortie spontanée", file.Path())
			}
			if signals := process.Signals(); len(signals) != 0 {
				t.Fatalf("signaux après sortie observée=%v", signals)
			}
			waitEmpty(t, root)
		})
	}
}

func TestStopExitRaceUsesAtomicWinner(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		root := t.TempDir()
		process := sysexec.NewFakeProcess()
		process.SignalFunc = func(os.Signal) error {
			process.Exit(errors.New("exit after SIGINT"))
			return nil
		}
		executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
			writeWAV(t, args[len(args)-1], 200*time.Millisecond)
			return process, nil
		}}
		recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
			Directory: root, ReadinessInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := receiveReady(t, recording.Ready()); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		type stopResult struct {
			file audio.WavFile
			err  error
		}
		stopped := make(chan stopResult, 1)
		go func() {
			<-start
			file, err := recording.Stop(context.Background())
			stopped <- stopResult{file: file, err: err}
		}()
		go func() {
			<-start
			process.Exit(errors.New("spontaneous exit"))
		}()
		close(start)
		result := <-stopped

		signals := process.Signals()
		switch errs.Classify(result.err) {
		case "":
			if result.file.Path() == "" || len(signals) != 1 || signals[0] != os.Interrupt {
				t.Fatalf("iteration %d: stop gagnant, file=%q signals=%v", iteration, result.file.Path(), signals)
			}
			if _, err := result.file.Release(); err != nil {
				t.Fatal(err)
			}
		case errs.AudioFailed:
			if result.file.Path() != "" || len(signals) != 0 {
				t.Fatalf("iteration %d: sortie gagnante, file=%q signals=%v", iteration, result.file.Path(), signals)
			}
		default:
			t.Fatalf("iteration %d: Stop() error=%v", iteration, result.err)
		}
		waitEmpty(t, root)
	}
}

func TestAbortConcurrentWithStopOnlyObservesOwner(t *testing.T) {
	root := t.TempDir()
	var path string
	signaled := make(chan struct{})
	release := make(chan struct{})
	process := sysexec.NewFakeProcess()
	process.SignalFunc = func(os.Signal) error {
		writeWAV(t, path, 200*time.Millisecond)
		close(signaled)
		<-release
		process.Exit(nil)
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		writeWAV(t, path, 200*time.Millisecond)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() {
		file, err := recording.Stop(context.Background())
		if file.Path() != "" {
			_, err = file.Release()
		}
		stopped <- err
	}()
	select {
	case <-signaled:
	case <-time.After(time.Second):
		t.Fatal("Stop propriétaire non démarré")
	}
	aborted := make(chan error, 1)
	go func() { aborted <- recording.Abort() }()
	select {
	case err := <-aborted:
		t.Fatalf("Abort waiter terminé avant le propriétaire: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if process.KillCount() != 0 {
		t.Fatalf("Abort waiter a tué le processus: kills=%d", process.KillCount())
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := <-aborted; err != nil {
		t.Fatal(err)
	}
	if signals := process.Signals(); len(signals) != 1 || signals[0] != os.Interrupt {
		t.Fatalf("signaux=%v", signals)
	}
	waitEmpty(t, root)
}

func TestCanceledStopWaiterDoesNotKillOwner(t *testing.T) {
	root := t.TempDir()
	var path string
	signaled := make(chan struct{})
	release := make(chan struct{})
	process := sysexec.NewFakeProcess()
	process.SignalFunc = func(os.Signal) error {
		writeWAV(t, path, 200*time.Millisecond)
		close(signaled)
		<-release
		process.Exit(nil)
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		writeWAV(t, path, 200*time.Millisecond)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() {
		file, err := recording.Stop(context.Background())
		if file.Path() != "" {
			_, err = file.Release()
		}
		stopped <- err
	}()
	select {
	case <-signaled:
	case <-time.After(time.Second):
		t.Fatal("Stop propriétaire non démarré")
	}
	waiterCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recording.Stop(waiterCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop waiter=%v", err)
	}
	if process.KillCount() != 0 {
		t.Fatalf("Stop waiter a tué le processus: kills=%d", process.KillCount())
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	waitEmpty(t, root)
}

func TestReadinessTimeoutKillsProcess(t *testing.T) {
	root := t.TempDir()
	process := sysexec.NewFakeProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		writeWAV(t, args[len(args)-1], 0)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessTimeout: 20 * time.Millisecond, ReadinessInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); errs.Classify(err) != errs.Timeout {
		t.Fatalf("Ready() = %v", err)
	}
	waitFor(t, func() bool { return process.KillCount() > 0 })
	waitEmpty(t, root)
}

func TestStopDetectsEmptyCaptureAndRetainsBoundedDiagnostic(t *testing.T) {
	root := privateTempDir(t)
	diagnostics := filepath.Join(root, "diagnostics")
	process := sysexec.NewFakeProcess()
	var path string
	process.SignalFunc = func(os.Signal) error {
		writeWAV(t, path, 20*time.Millisecond)
		process.Exit(nil)
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		writeWAV(t, path, 20*time.Millisecond)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond, MinimumDuration: 100 * time.Millisecond,
		Retention: audio.RetentionOptions{Keep: true, Directory: diagnostics, MaxFiles: 1, MaxBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	_, err = recording.Stop(context.Background())
	if errs.Classify(err) != errs.NoSpeech {
		t.Fatalf("Stop() = %v", err)
	}
	entries, readErr := os.ReadDir(diagnostics)
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("diagnostics = %v, %v", entries, readErr)
	}
}

func TestAbortKillsAndRemovesEvenWithRetention(t *testing.T) {
	root := t.TempDir()
	process := sysexec.NewFakeProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		writeWAV(t, args[len(args)-1], 200*time.Millisecond)
		return process, nil
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond,
		Retention: audio.RetentionOptions{Keep: true, Directory: filepath.Join(root, "diagnostics")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	if err := recording.Abort(); err != nil {
		t.Fatal(err)
	}
	if process.KillCount() != 1 {
		t.Fatalf("KillCount() = %d", process.KillCount())
	}
	waitEmpty(t, root)
}

func TestContextCancellationKillsWithoutOrphan(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	process := sysexec.NewFakeProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		writeWAV(t, args[len(args)-1], 200*time.Millisecond)
		return process, nil
	}}
	recording, err := New(executor).Start(ctx, audio.RecordOptions{Directory: root, ReadinessInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitFor(t, func() bool { return process.KillCount() > 0 })
	waitEmpty(t, root)
}

func TestOSExecutorStopUsesTrapInstalledBeforeReadinessAndLeavesNoDescendant(t *testing.T) {
	root := t.TempDir()
	captureRoot := t.TempDir()
	tools := t.TempDir()
	fixture := filepath.Join(root, "fixture.wav")
	writeWAV(t, fixture, 200*time.Millisecond)
	childPIDFile := filepath.Join(root, "child.pid")
	readyGate := filepath.Join(root, "allow-ready")
	pathEnv := tools + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	writeExecutable(t, filepath.Join(tools, "pw-record"), `#!/bin/sh
for arg do out="$arg"; done
trap 'kill "$child" 2>/dev/null; wait "$child" 2>/dev/null; exit 0' INT TERM
sleep 30 &
child=$!
tmp="$MURMUR_CHILD_PID.tmp.$$"
printf '%s' "$child" >"$tmp"
mv "$tmp" "$MURMUR_CHILD_PID"
while [ ! -e "$MURMUR_READY_GATE" ]; do sleep 0.01; done
cp "$MURMUR_FIXTURE" "$out"
wait "$child"
`)
	executor := sysexec.OSExecutor{Env: []string{
		"PATH=" + pathEnv,
		"MURMUR_CHILD_PID=" + childPIDFile,
		"MURMUR_READY_GATE=" + readyGate,
		"MURMUR_FIXTURE=" + fixture,
	}}
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: captureRoot, ReadinessInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitPIDFile(t, childPIDFile)
	select {
	case err := <-recording.Ready():
		t.Fatalf("readiness avant barrière: %v", err)
	default:
	}
	if err := os.WriteFile(readyGate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	file, err := recording.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Release(); err != nil {
		t.Fatal(err)
	}
	awaitDone(t, recording.Done())
	waitProcessGone(t, childPID)
	waitEmpty(t, captureRoot)
}

func TestStartFailureLeavesNoTemporaryDirectory(t *testing.T) {
	root := t.TempDir()
	executor := &sysexec.FakeExecutor{StartFunc: func(context.Context, string, []string) (sysexec.Process, error) {
		return nil, errors.New("missing")
	}}
	_, err := New(executor).Start(context.Background(), audio.RecordOptions{Directory: root})
	if errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("Start() = %v", err)
	}
	waitEmpty(t, root)
}

func TestStopAndAbortRemainBoundedWhenProcessNeverExits(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*recording) error
	}{
		{name: "stop", run: func(recording *recording) error {
			_, err := recording.Stop(context.Background())
			return err
		}},
		{name: "abort", run: func(recording *recording) error { return recording.Abort() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			process := sysexec.NewFakeProcess()
			process.KillFunc = func() error { return nil }
			recording := &recording{
				process: process, tempDir: t.TempDir(), opts: audio.RecordOptions{},
				stopGrace: 10 * time.Millisecond, abortGrace: 10 * time.Millisecond, killWait: 10 * time.Millisecond,
				ready: make(chan error), procDone: make(chan struct{}), finished: make(chan struct{}),
			}
			started := time.Now()
			err := test.run(recording)
			if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
				t.Fatalf("operation blocked for %s", elapsed)
			}
			if errs.Classify(err) != errs.Timeout {
				t.Fatalf("error = %v", err)
			}
			if signals := process.Signals(); len(signals) != 1 || signals[0] != os.Interrupt || process.KillCount() != 1 {
				t.Fatalf("signals = %v, kills = %d", signals, process.KillCount())
			}
		})
	}
}

func TestLiveSingleProcessGrowingWAVHasExactPCMAndMergesTail(t *testing.T) {
	root := t.TempDir()
	const totalSamples = 740
	const stepSamples = 80
	var path string
	produced := make(chan struct{})
	process := sysexec.NewFakeProcess()
	process.SignalFunc = func(os.Signal) error {
		<-produced
		if _, err := wav.FinalizeGrowing(path); err != nil {
			return err
		}
		process.Exit(nil)
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		go func() {
			defer close(produced)
			// A real pw-record returns from Start before creating its output.
			time.Sleep(5 * time.Millisecond)
			if err := wav.CreateGrowing(path); err != nil {
				t.Errorf("create growing WAV: %v", err)
				return
			}
			for start := 0; start < totalSamples; start += stepSamples {
				count := stepSamples
				if start+count > totalSamples {
					count = totalSamples - start
				}
				data := make([]byte, count*wav.BlockAlign)
				for index := range count {
					binary.LittleEndian.PutUint16(data[index*2:index*2+2], uint16(int16((start+index)%32767)))
				}
				if err := wav.AppendPCM(path, data); err != nil {
					t.Errorf("append PCM: %v", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		return process, nil
	}}
	recording, err := New(executor).StartLive(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond, MinimumDuration: time.Millisecond,
	}, audio.LiveOptions{SegmentDuration: 20 * time.Millisecond, MinimumTail: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	var segments []audio.LiveSegment
	select {
	case segment := <-recording.Segments():
		segments = append(segments, segment)
	case <-time.After(time.Second):
		t.Fatal("aucun segment émis pendant la capture")
	}
	if err := recording.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitDone(t, recording.Done())
	if err := recording.Err(); err != nil {
		t.Fatalf("Err() après stop live = %v", err)
	}
	for segment := range recording.Segments() {
		segments = append(segments, segment)
	}
	if len(segments) != 2 {
		t.Fatalf("segments=%#v", segments)
	}
	if segments[0].File.Samples() != 320 || segments[1].File.Samples() != 420 {
		t.Fatalf("échantillons=%d,%d", segments[0].File.Samples(), segments[1].File.Samples())
	}
	final, err := wav.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, segment := range segments {
		joined = append(joined, readWAVData(t, segment.File)...)
	}
	if want := readWAVData(t, final); !bytes.Equal(joined, want) {
		t.Fatalf("PCM segmenté=%d final=%d", len(joined), len(want))
	}
	if calls := executor.Calls(); len(calls) != 1 || calls[0].Name != "pw-record" {
		t.Fatalf("processus lancés=%#v", calls)
	}
	if len(process.Signals()) != 1 {
		t.Fatalf("signaux=%v", process.Signals())
	}
	if _, err := recording.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := recording.Release(); err != nil {
		t.Fatal(err)
	}
	waitEmpty(t, root)
}

func TestLiveStopSignalsBeforeBlockedSTTDrainsAndPreservesPCM(t *testing.T) {
	root := t.TempDir()
	const totalSamples = 160
	const segmentSamples = 16
	pcm := make([]byte, totalSamples*wav.BlockAlign)
	for index := range totalSamples {
		binary.LittleEndian.PutUint16(pcm[index*wav.BlockAlign:], uint16(index+1))
	}

	var path string
	signaled := make(chan struct{})
	process := sysexec.NewFakeProcess()
	process.SignalFunc = func(signal os.Signal) error {
		if signal != os.Interrupt {
			return errors.New("signal inattendu")
		}
		close(signaled)
		final, err := wav.FinalizeGrowing(path)
		if err != nil {
			return err
		}
		if err := final.Close(); err != nil {
			return err
		}
		process.Exit(nil)
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		if err := wav.CreateGrowing(path); err != nil {
			return nil, err
		}
		if err := wav.AppendPCM(path, pcm[:wav.BlockAlign]); err != nil {
			return nil, err
		}
		return process, nil
	}}
	recording, err := New(executor).StartLive(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond, MinimumDuration: time.Millisecond,
	}, audio.LiveOptions{SegmentDuration: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	provider := &blockedSTTProvider{gate: gate, entered: make(chan int, totalSamples/segmentSamples), files: make(map[int]wav.File)}
	chain, err := stt.NewChain(provider, nil, stt.MultiOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan stt.Segment)
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		defer close(requests)
		for segment := range recording.Segments() {
			requests <- stt.Segment{Index: segment.Index, Request: stt.Request{
				Audio: segment.File, Filename: filepath.Base(segment.File.Path()), Model: strconv.Itoa(segment.Index),
			}}
		}
	}()
	type transcriptionResult struct {
		result stt.MultiResult
		err    error
	}
	transcribed := make(chan transcriptionResult, 1)
	go func() {
		result, err := chain.TranscribeStream(context.Background(), requests, stt.Progress{})
		transcribed <- transcriptionResult{result: result, err: err}
	}()

	if err := wav.AppendPCM(path, pcm[wav.BlockAlign:]); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-provider.entered:
		case <-time.After(time.Second):
			t.Fatal("workers STT non bloqués")
		}
	}
	live := recording.(*liveRecording)
	waitFor(t, func() bool { return len(live.parts) == cap(live.parts) })

	stopDone := make(chan error, 1)
	go func() { stopDone <- recording.Stop(context.Background()) }()
	select {
	case <-signaled:
	case <-time.After(time.Second):
		t.Fatal("SIGINT bloqué par la file de segments pleine")
	}
	select {
	case err := <-stopDone:
		t.Fatalf("Stop a finalisé avant le drainage STT: %v", err)
	default:
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	cancelWaiter()
	if err := recording.Stop(waiterCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter Stop()=%v", err)
	}
	select {
	case err := <-stopDone:
		t.Fatalf("le waiter a annulé le Stop propriétaire: %v", err)
	default:
	}
	close(gate)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop non finalisé après drainage STT")
	}
	awaitDone(t, forwarded)

	var outcome transcriptionResult
	select {
	case outcome = <-transcribed:
	case <-time.After(time.Second):
		t.Fatal("pool STT non terminé")
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	wantSegments := totalSamples / segmentSamples
	if len(outcome.result.Segments) != wantSegments {
		t.Fatalf("segments STT=%d, want %d", len(outcome.result.Segments), wantSegments)
	}
	provider.mu.Lock()
	files := make(map[int]wav.File, len(provider.files))
	for index, file := range provider.files {
		files[index] = file
	}
	provider.mu.Unlock()
	var joined []byte
	for index, segment := range outcome.result.Segments {
		if segment.Index != index {
			t.Fatalf("segment[%d].Index=%d", index, segment.Index)
		}
		file, ok := files[index]
		if !ok {
			t.Fatalf("segment PCM %d absent", index)
		}
		joined = append(joined, readWAVData(t, file)...)
	}
	if !bytes.Equal(joined, pcm) {
		t.Fatalf("PCM segmenté=%d octets, want %d", len(joined), len(pcm))
	}
	if signals := process.Signals(); len(signals) != 1 || signals[0] != os.Interrupt {
		t.Fatalf("signaux=%v", signals)
	}
	if _, err := recording.Release(); err != nil {
		t.Fatal(err)
	}
	waitEmpty(t, root)
}

type blockedSTTProvider struct {
	gate    <-chan struct{}
	entered chan int
	mu      sync.Mutex
	files   map[int]wav.File
}

func (p *blockedSTTProvider) Name() string { return "blocked" }

func (p *blockedSTTProvider) Transcribe(ctx context.Context, request stt.Request) (stt.Result, error) {
	index, err := strconv.Atoi(request.Model)
	if err != nil {
		return stt.Result{}, err
	}
	p.mu.Lock()
	p.files[index] = request.Audio
	p.mu.Unlock()
	p.entered <- index
	select {
	case <-p.gate:
		return stt.Result{Text: request.Model}, nil
	case <-ctx.Done():
		return stt.Result{}, ctx.Err()
	}
}

func TestLiveAbortLeavesNoProcessOrCaptureWithRetention(t *testing.T) {
	root := t.TempDir()
	process := sysexec.NewFakeProcess()
	var path string
	process.SignalFunc = func(os.Signal) error {
		process.Exit(nil)
		return nil
	}
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		if err := wav.CreateGrowing(path); err != nil {
			return nil, err
		}
		if err := wav.AppendPCM(path, []byte{0, 0}); err != nil {
			return nil, err
		}
		return process, nil
	}}
	recording, err := New(executor).StartLive(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond,
		Retention: audio.RetentionOptions{Keep: true, Directory: filepath.Join(root, "diagnostics")},
	}, audio.LiveOptions{SegmentDuration: time.Second, MinimumTail: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	if err := recording.Abort(); err != nil {
		t.Fatal(err)
	}
	if len(process.Signals()) != 1 {
		t.Fatalf("processus enfant non arrêté: signaux=%v kills=%d", process.Signals(), process.KillCount())
	}
	if len(executor.Calls()) != 1 {
		t.Fatalf("nombre de pw-record=%d", len(executor.Calls()))
	}
	waitEmpty(t, root)
}

func TestLiveDiscardAfterStopIgnoresRetention(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "capture")
	if err := os.Mkdir(capture, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(capture, "recording.wav")
	writeWAV(t, path, 200*time.Millisecond)
	file, err := wav.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := filepath.Join(root, "diagnostics")
	done := make(chan struct{})
	close(done)
	recording := &liveRecording{
		tempDir: capture, done: done,
		final: audio.NewWavFile(file, capture, audio.RetentionOptions{Keep: true, Directory: diagnostics}),
	}
	if err := recording.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := recording.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("capture still present: %v", err)
	}
	if _, err := os.Stat(diagnostics); !os.IsNotExist(err) {
		t.Fatalf("diagnostic directory created: %v", err)
	}
}

func TestLiveAbortClosesFinalSnapshotAfterFinalizeError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "recording.wav")
	writeWAV(t, path, 200*time.Millisecond)
	file, err := wav.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	final := audio.NewWavFile(file, root, audio.RetentionOptions{})
	segmenter, err := audio.NewGrowingSegmenter(filepath.Join(root, "other.wav"), root, audio.LiveOptions{
		SegmentDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	recording := &liveRecording{
		segmenter: segmenter, tempDir: root, ctx: ctx, cancel: cancel,
		parts: make(chan audio.LiveSegment, 1), done: make(chan struct{}),
	}
	if err := recording.finalize(liveCaptureResult{file: final}); err == nil {
		t.Fatal("finalisation invalide acceptée")
	}
	close(recording.done)
	if err := recording.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, handle, err := file.OpenRIFF(); err == nil {
		handle.Close()
		t.Fatal("snapshot final encore ouvert après Abort")
	}
}

func TestLiveDoneReportsProcessDeathAfterReadiness(t *testing.T) {
	root := t.TempDir()
	process := sysexec.NewFakeProcess()
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path := args[len(args)-1]
		if err := wav.CreateGrowing(path); err != nil {
			return nil, err
		}
		if err := wav.AppendPCM(path, make([]byte, 200*wav.BlockAlign)); err != nil {
			return nil, err
		}
		return process, nil
	}}
	recording, err := New(executor).StartLive(context.Background(), audio.RecordOptions{
		Directory: root, ReadinessInterval: time.Millisecond,
	}, audio.LiveOptions{SegmentDuration: time.Second, MinimumTail: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	process.Exit(errors.New("boom"))
	awaitDone(t, recording.Done())
	if err := recording.Err(); errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("Err() = %v", err)
	}
	_ = recording.Abort()
	waitEmpty(t, root)
}

func readWAVData(t *testing.T, file wav.File) []byte {
	t.Helper()
	reader, handle, err := file.OpenData()
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := handle.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("lecture PCM: %v, %v", readErr, closeErr)
	}
	return data
}

func receiveReady(t *testing.T, ready <-chan error) error {
	t.Helper()
	select {
	case err := <-ready:
		return err
	case <-time.After(time.Second):
		t.Fatal("timeout en attente de readiness")
		return nil
	}
}

func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout en attente de terminaison")
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		parsed, err := strconv.Atoi(string(data))
		if err != nil {
			t.Fatal(err)
		}
		pid = parsed
		return pid > 0
	})
	return pid
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, func() bool {
		err := syscall.Kill(pid, 0)
		return errors.Is(err, syscall.ESRCH)
	})
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func waitEmpty(t *testing.T, path string) {
	t.Helper()
	waitFor(t, func() bool {
		entries, err := os.ReadDir(path)
		return err == nil && len(entries) == 0
	})
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition non satisfaite")
}

var wavWriteMu sync.Mutex

func writeWAV(t *testing.T, path string, duration time.Duration) {
	t.Helper()
	wavWriteMu.Lock()
	defer wavWriteMu.Unlock()
	sampleCount := int64(duration) * wav.SampleRate / int64(time.Second)
	dataSize := sampleCount * wav.BlockAlign
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(dataSize+36))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], wav.Channels)
	binary.LittleEndian.PutUint32(header[24:28], wav.SampleRate)
	binary.LittleEndian.PutUint32(header[28:32], wav.ByteRate)
	binary.LittleEndian.PutUint16(header[32:34], wav.BlockAlign)
	binary.LittleEndian.PutUint16(header[34:36], wav.BitsPerSample)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))
	content := append(header, make([]byte, dataSize)...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendSample(t *testing.T, path string) {
	t.Helper()
	wavWriteMu.Lock()
	defer wavWriteMu.Unlock()
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}
