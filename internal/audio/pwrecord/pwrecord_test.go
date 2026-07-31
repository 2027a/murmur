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
	"sync"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/sysexec"
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
	case <-time.After(30 * time.Millisecond):
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
	root := t.TempDir()
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
		if err := wav.CreateGrowing(path); err != nil {
			return nil, err
		}
		go func() {
			defer close(produced)
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
