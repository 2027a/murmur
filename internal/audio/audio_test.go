package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/xdg"
)

func TestFakeRecorderStopAndDefaultCleanup(t *testing.T) {
	fixture := audioFixture(t, time.Second)
	recorder := &FakeRecorder{FixturePath: fixture}
	recording, err := recorder.Start(context.Background(), RecordOptions{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-recording.Ready(); err != nil {
		t.Fatal(err)
	}
	file, err := recording.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if file.Duration() != time.Second {
		t.Fatalf("Duration() = %v", file.Duration())
	}
	path := file.Path()
	if _, err := file.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture temporaire encore présente: %v", err)
	}
}

func TestWavFileDiscardAfterStopNeverRetains(t *testing.T) {
	root := t.TempDir()
	diagnostics := filepath.Join(root, "diagnostics")
	recording, err := (&FakeRecorder{FixturePath: audioFixture(t, time.Second)}).Start(context.Background(), RecordOptions{
		Directory: root, Retention: RetentionOptions{Keep: true, Directory: diagnostics},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-recording.Ready(); err != nil {
		t.Fatal(err)
	}
	file, err := recording.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := file.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture still present: %v", err)
	}
	if _, err := os.Stat(diagnostics); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diagnostic directory created: %v", err)
	}
}

func TestFakeRecorderAbortAlwaysRemoves(t *testing.T) {
	recorder := &FakeRecorder{FixturePath: audioFixture(t, time.Second)}
	root := t.TempDir()
	recording, err := recorder.Start(context.Background(), RecordOptions{
		Directory: root,
		Retention: RetentionOptions{Keep: true, Directory: filepath.Join(root, "diagnostics")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recording.Abort(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("résidus après Abort: %v", entries)
	}
}

func TestFakeRecorderContextCancellationRemovesCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()
	recorder := &FakeRecorder{FixturePath: audioFixture(t, time.Second)}
	_, err := recorder.Start(ctx, RecordOptions{Directory: root})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		entries, readErr := os.ReadDir(root)
		if readErr == nil && len(entries) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("la fixture a survécu à l'annulation")
}

func TestFakeRecorderEmptyCapture(t *testing.T) {
	recorder := &FakeRecorder{FixturePath: audioFixture(t, 50*time.Millisecond)}
	recording, err := recorder.Start(context.Background(), RecordOptions{Directory: t.TempDir(), MinimumDuration: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = recording.Stop(context.Background())
	if errs.Classify(err) != errs.NoSpeech {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestRetentionIsBounded(t *testing.T) {
	diagnostics := filepath.Join(privateTempDir(t), "diagnostics")
	for i := 0; i < 4; i++ {
		recorder := &FakeRecorder{FixturePath: audioFixture(t, 120*time.Millisecond)}
		recording, err := recorder.Start(context.Background(), RecordOptions{
			Directory: t.TempDir(),
			Retention: RetentionOptions{Keep: true, Directory: diagnostics, MaxFiles: 2, MaxBytes: 1 << 20},
		})
		if err != nil {
			t.Fatal(err)
		}
		file, err := recording.Stop(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		destination, err := file.Release()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		relative, err := filepath.Rel(diagnostics, destination)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("destination hors store: %q, %v", destination, err)
		}
		info, err := os.Lstat(destination)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("destination privée invalide: %v, %v", info, err)
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := os.ReadDir(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("diagnostics conservés = %d, attendu 2", len(entries))
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions %s = %o", entry.Name(), info.Mode().Perm())
		}
	}
	info, err := os.Lstat(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		t.Fatalf("store de rétention = %v", info.Mode())
	}
}

func TestWavFileReleaseRetainsValidatedSnapshotBytes(t *testing.T) {
	root := privateTempDir(t)
	captureDir := filepath.Join(root, "capture")
	if err := os.Mkdir(captureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(captureDir, "recording.wav")
	validated := audioFixture(t, 120*time.Millisecond)
	original, err := os.ReadFile(validated)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, append(append([]byte(nil), original...), []byte("physical suffix")...), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := wav.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	mutated := append([]byte(nil), original...)
	for index := 44; index < len(mutated); index++ {
		mutated[index] ^= 0xff
	}
	if err := os.WriteFile(source, append(mutated, []byte("mutated suffix")...), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics := filepath.Join(root, "diagnostics")
	retained, err := NewWavFile(file, captureDir, RetentionOptions{
		Keep: true, Directory: diagnostics, MaxFiles: 10, MaxBytes: 1 << 20,
	}).Release()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(retained)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("rétention=%d octets, snapshot validé=%d", len(data), len(original))
	}
	if bytes.Contains(data, []byte("suffix")) {
		t.Fatal("la rétention a conservé un suffixe physique hors RIFF")
	}
}

func TestRetentionRejectsRelativeBase(t *testing.T) {
	for _, test := range []struct {
		name    string
		options RetentionOptions
	}{
		{name: "XDG_STATE_HOME", options: RetentionOptions{Keep: true}},
		{name: "explicit", options: RetentionOptions{Keep: true, Directory: "relative/recordings"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", "relative/state")
			tempDir := t.TempDir()
			source := filepath.Join(tempDir, "recording.wav")
			if err := os.WriteFile(source, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := RetainCapture(source, tempDir, test.options); err == nil {
				t.Fatal("relative retention base accepted")
			}
			if _, err := os.Stat(tempDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary capture not removed: %v", err)
			}
		})
	}
}

func TestRetentionRejectsSymlinksWithoutChangingTarget(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory func(root, link string) string
	}{
		{name: "intermediate", directory: func(root, link string) string { return filepath.Join(root, link, "recordings") }},
		{name: "final", directory: func(root, link string) string { return filepath.Join(root, link) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			target := t.TempDir()
			if err := os.Chmod(target, 0o750); err != nil {
				t.Fatal(err)
			}
			link := "linked"
			if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
				t.Fatal(err)
			}
			tempDir := t.TempDir()
			source := filepath.Join(tempDir, "recording.wav")
			if err := os.WriteFile(source, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := RetainCapture(source, tempDir, RetentionOptions{Keep: true, Directory: test.directory(root, link)})
			if err == nil {
				t.Fatal("symbolic link accepted")
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o750 {
				t.Fatalf("target mode changed to %o", info.Mode().Perm())
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("target received retained files: %v, %v", entries, err)
			}
		})
	}
}

func TestRetentionRejectsExistingUnsafeModeWithoutChmod(t *testing.T) {
	for _, test := range []struct {
		name      string
		setupPath func(root string) (string, string)
	}{
		{name: "final", setupPath: func(root string) (string, string) { return root, root }},
		{name: "parent", setupPath: func(root string) (string, string) {
			return filepath.Join(root, "recordings"), root
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			directory, unsafePath := test.setupPath(root)
			if err := os.Chmod(unsafePath, 0o770); err != nil {
				t.Fatal(err)
			}
			tempDir := t.TempDir()
			source := filepath.Join(tempDir, "recording.wav")
			if err := os.WriteFile(source, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := RetainCapture(source, tempDir, RetentionOptions{Keep: true, Directory: directory}); err == nil {
				t.Fatal("unsafe directory mode accepted")
			}
			info, err := os.Stat(unsafePath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o770 {
				t.Fatalf("unsafe directory mode changed to %o", info.Mode().Perm())
			}
		})
	}
}

func TestRetentionSubstitutionDoesNotEscapeValidatedStore(t *testing.T) {
	root := privateTempDir(t)
	storePath := filepath.Join(root, "recordings")
	store, err := xdg.OpenPrivateDir(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	moved := filepath.Join(root, "validated")
	if err := os.Rename(storePath, moved); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, storePath); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source.wav")
	if err := os.WriteFile(source, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = retainInStore(source, store, RetentionOptions{MaxFiles: 10, MaxBytes: 1 << 20})
	if err == nil {
		t.Fatal("store substitution not detected")
	}
	for _, directory := range []string{moved, replacement} {
		entries, readErr := os.ReadDir(directory)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("retained file escaped or remained in %s: %v, %v", directory, entries, readErr)
		}
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source removed before store validation: %v", err)
	}
}

func TestRetentionSourceSubstitutionLeavesReplacementUntouched(t *testing.T) {
	sourceRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "recording.wav")
	if err := os.WriteFile(source, []byte("original-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics := filepath.Join(privateTempDir(t), "diagnostics")
	reached := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	defer unblock()
	originalHook := beforeRetentionSourceRemove
	beforeRetentionSourceRemove = func() {
		close(reached)
		<-resume
	}
	defer func() { beforeRetentionSourceRemove = originalHook }()

	result := make(chan error, 1)
	go func() {
		_, err := RetainCapture(source, sourceRoot, RetentionOptions{
			Keep: true, Directory: diagnostics, MaxFiles: 10, MaxBytes: 1024,
		})
		result <- err
	}()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("copie de rétention non terminée")
	}
	replacement := filepath.Join(t.TempDir(), "replacement.wav")
	if err := os.WriteFile(replacement, []byte("replacement-must-survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-result:
		if errs.Classify(err) != errs.AudioFailed || !errors.Is(err, errRetentionSourceChanged) {
			t.Fatalf("RetainCapture() error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rétention non terminée après substitution")
	}
	data, err := os.ReadFile(source)
	if err != nil || string(data) != "replacement-must-survive" {
		t.Fatalf("remplacement=%q error=%v", data, err)
	}
	entries, err := os.ReadDir(diagnostics)
	if err != nil || len(entries) != 0 {
		t.Fatalf("diagnostic conservé après substitution: %v, %v", entries, err)
	}
}

func TestRetentionRejectsNonRegularSourceWithoutBlocking(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "fifo", setup: func(path string) error { return syscall.Mkfifo(path, 0o600) }},
		{name: "symlink", setup: func(path string) error {
			target := path + ".target"
			if err := os.WriteFile(target, []byte("audio"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "recording.wav")
			if err := test.setup(source); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := RetainCapture(source, "", RetentionOptions{
					Keep: true, Directory: filepath.Join(root, "diagnostics"), MaxBytes: 1024,
				})
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("source non régulière acceptée")
				}
			case <-time.After(time.Second):
				t.Fatal("rétention bloquée sur une source non régulière")
			}
		})
	}
}

func TestRetentionNeverPrunesCurrentCapture(t *testing.T) {
	diagnostics := filepath.Join(privateTempDir(t), "diagnostics")
	retain := func(content string) string {
		t.Helper()
		root := t.TempDir()
		source := filepath.Join(root, "recording.wav")
		if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		destination, err := RetainCapture(source, root, RetentionOptions{
			Keep: true, Directory: diagnostics, MaxFiles: 1, MaxBytes: 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		return destination
	}
	first := retain("first")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(first, future, future); err != nil {
		t.Fatal(err)
	}
	second := retain("second")
	if data, err := os.ReadFile(second); err != nil || string(data) != "second" {
		t.Fatalf("capture courante=%q error=%v", data, err)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ancienne capture non supprimée: %v", err)
	}
}

func TestRetentionConcurrentTransactionsRemainBounded(t *testing.T) {
	diagnostics := filepath.Join(privateTempDir(t), "diagnostics")
	const workers = 24
	start := make(chan struct{})
	errorsSeen := make(chan error, workers)
	var wait sync.WaitGroup
	roots := make([]string, workers)
	for index := range roots {
		roots[index] = t.TempDir()
	}
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			root := roots[index]
			source := filepath.Join(root, "recording.wav")
			if err := os.WriteFile(source, []byte{byte(index)}, 0o600); err != nil {
				errorsSeen <- err
				return
			}
			<-start
			_, err := RetainCapture(source, root, RetentionOptions{
				Keep: true, Directory: diagnostics, MaxFiles: 3, MaxBytes: 1024,
			})
			errorsSeen <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("captures après concurrence=%d, want 3", len(entries))
	}
}

func TestChunkOrderTailAndCleanup(t *testing.T) {
	file, err := wav.Open(audioFixture(t, 25*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	segments, err := Chunk(file, ChunkOptions{Duration: 10 * time.Second, MinTail: 6 * time.Second, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].Index != 0 || segments[1].Index != 1 {
		t.Fatalf("segments = %#v", segments)
	}
	if segments[0].File.Duration() != 10*time.Second || segments[1].File.Duration() != 15*time.Second {
		t.Fatalf("durées = %v, %v", segments[0].File.Duration(), segments[1].File.Duration())
	}
	paths := []string{segments[0].File.Path(), segments[1].File.Path()}
	if err := RemoveSegments(segments); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("segment non supprimé: %s", path)
		}
	}
}

func TestChunkDisabledAndShortPathDoNotCopy(t *testing.T) {
	file, err := wav.Open(audioFixture(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, duration := range []time.Duration{0, 2 * time.Second} {
		output := t.TempDir()
		segments, err := Chunk(file, ChunkOptions{Duration: duration, OutputDir: output})
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) != 1 || segments[0].Temporary || segments[0].File.Path() != file.Path() {
			t.Fatalf("Chunk(%v) = %#v", duration, segments)
		}
		entries, err := os.ReadDir(output)
		if err != nil || len(entries) != 0 {
			t.Fatalf("sortie chemin court = %v, %v", entries, err)
		}
	}
}

func TestFakeLiveSnapshotsMergeTailWithoutLosingSamples(t *testing.T) {
	fixture := audioFixture(t, 2500*time.Millisecond)
	root := t.TempDir()
	recording, err := (&FakeRecorder{FixturePath: fixture}).StartLive(context.Background(), RecordOptions{Directory: root}, LiveOptions{
		SegmentDuration: time.Second,
		MinimumTail:     time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-recording.Ready(); err != nil {
		t.Fatal(err)
	}
	first := <-recording.Segments()
	if first.Index != 0 || first.File.Duration() != time.Second {
		t.Fatalf("premier segment=%#v durée=%v", first, first.File.Duration())
	}
	if err := recording.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, ok := <-recording.Segments()
	if !ok || second.Index != 1 || second.File.Duration() != 1500*time.Millisecond {
		t.Fatalf("dernier segment=%#v ok=%v durée=%v", second, ok, second.File.Duration())
	}
	source, err := wav.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var joined bytes.Buffer
	for _, file := range []wav.File{first.File, second.File} {
		reader, handle, err := file.OpenData()
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := joined.ReadFrom(reader)
		closeErr := handle.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("lecture segment: %v, %v", copyErr, closeErr)
		}
	}
	reader, handle, err := source.OpenData()
	if err != nil {
		t.Fatal(err)
	}
	want := new(bytes.Buffer)
	_, copyErr := want.ReadFrom(reader)
	closeErr := handle.Close()
	if copyErr != nil || closeErr != nil || !bytes.Equal(joined.Bytes(), want.Bytes()) {
		t.Fatalf("reconcaténation différente: copy=%v close=%v", copyErr, closeErr)
	}
	if _, err := recording.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := recording.Release(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cleanup global: %v, %v", entries, err)
	}
}

func TestGrowingSegmenterSnapshotsPinnedDescriptorAfterPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recording.wav")
	if err := wav.CreateGrowing(path); err != nil {
		t.Fatal(err)
	}
	source, err := wav.OpenGrowing(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "original.wav")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	replacement := audioFixture(t, time.Second)
	replacementData, err := os.ReadFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacementData, 0o600); err != nil {
		t.Fatal(err)
	}
	segmenter, err := NewGrowingSegmenterFromFile(source, path, dir, LiveOptions{SegmentDuration: time.Second / wav.SampleRate})
	if err != nil {
		t.Fatal(err)
	}
	defer segmenter.Close()

	originalPCM := []byte{0x11, 0x22, 0x33, 0x44}
	if _, err := writer.Write(originalPCM); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	segments, err := segmenter.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || !bytes.Equal(readWAVFileData(t, segments[0].File), originalPCM[:wav.BlockAlign]) {
		t.Fatalf("segment depuis chemin remplacé: %#v data=%v", segments, readWAVFileData(t, segments[0].File))
	}
}

func TestFakeLiveStopBeforeReadinessDoesNotDeadlock(t *testing.T) {
	gate := make(chan struct{})
	root := t.TempDir()
	recording, err := (&FakeRecorder{
		FixturePath: audioFixture(t, 200*time.Millisecond), ReadyGate: gate,
	}).StartLive(context.Background(), RecordOptions{
		Directory: root, MinimumDuration: time.Millisecond,
	}, LiveOptions{SegmentDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- recording.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop bloqué derrière ReadyGate")
	}
	if err := <-recording.Ready(); err != nil {
		t.Fatalf("Ready()=%v", err)
	}
	segments := 0
	for range recording.Segments() {
		segments++
	}
	if segments != 1 {
		t.Fatalf("segments après Stop pré-readiness=%d", segments)
	}
	if _, err := recording.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestFakeLiveCancelBeforeReadinessDoesNotDeadlock(t *testing.T) {
	gate := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	recording, err := (&FakeRecorder{
		FixturePath: audioFixture(t, 200*time.Millisecond), ReadyGate: gate,
	}).StartLive(ctx, RecordOptions{Directory: t.TempDir()}, LiveOptions{SegmentDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-recording.Done():
	case <-time.After(time.Second):
		t.Fatal("annulation bloquée derrière ReadyGate")
	}
	if err := recording.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err()=%v", err)
	}
	if err := recording.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestFakeLiveCanceledStopWaiterDoesNotCancelOwner(t *testing.T) {
	recording, err := (&FakeRecorder{FixturePath: audioFixture(t, 100*time.Millisecond)}).StartLive(
		context.Background(), RecordOptions{Directory: t.TempDir(), MinimumDuration: time.Millisecond},
		LiveOptions{SegmentDuration: time.Millisecond},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-recording.Ready(); err != nil {
		t.Fatal(err)
	}
	live := recording.(*fakeLiveRecording)
	deadline := time.Now().Add(time.Second)
	for len(live.segments) != cap(live.segments) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- recording.Stop(context.Background()) }()
	deadline = time.Now().Add(time.Second)
	for !live.stopRequested() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !live.stopRequested() {
		t.Fatal("Stop propriétaire non démarré")
	}
	waiterCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recording.Stop(waiterCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter Stop()=%v", err)
	}
	select {
	case err := <-ownerDone:
		t.Fatalf("le waiter a terminé le propriétaire: %v", err)
	default:
	}
	drained := make(chan struct{})
	go func() {
		for range recording.Segments() {
		}
		close(drained)
	}()
	if err := <-ownerDone; err != nil {
		t.Fatal(err)
	}
	<-drained
	if _, err := recording.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestFakeLiveFinalizeErrorClosesFinalSnapshot(t *testing.T) {
	file, err := wav.Open(audioFixture(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	segmenter, err := NewGrowingSegmenter(filepath.Join(t.TempDir(), "missing.wav"), t.TempDir(), LiveOptions{
		SegmentDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recording := &fakeLiveRecording{
		ctx: ctx, segmenter: segmenter, segments: make(chan LiveSegment, 1),
	}
	if err := recording.finalize(fakeStopRequest{file: file}); err == nil {
		t.Fatal("finalisation invalide acceptée")
	}
	if _, handle, err := file.OpenRIFF(); err == nil {
		handle.Close()
		t.Fatal("snapshot final encore ouvert après erreur")
	}
}

func audioFixture(t *testing.T, duration time.Duration) string {
	t.Helper()
	samples := int64(duration) * wav.SampleRate / int64(time.Second)
	dataSize := samples * wav.BlockAlign
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
	payload := make([]byte, dataSize)
	for i := int64(0); i < samples; i++ {
		binary.LittleEndian.PutUint16(payload[i*2:i*2+2], uint16(int16(i%32767)))
	}
	content := append(header, payload...)
	path := filepath.Join(t.TempDir(), "fixture.wav")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readWAVFileData(t *testing.T, file wav.File) []byte {
	t.Helper()
	reader, handle, err := file.OpenData()
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := handle.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("lecture WAV: %v, %v", readErr, closeErr)
	}
	return data
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
