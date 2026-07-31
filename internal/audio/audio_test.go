package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
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
	diagnostics := t.TempDir()
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
		if _, err := file.Release(); err != nil {
			t.Fatal(err)
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
