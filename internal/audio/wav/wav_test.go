package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

func TestOpenUnknownChunksPaddingAndDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.wav")
	audio := samples(1600)
	content := riff(
		chunk("JUNK", []byte{1, 2, 3}),
		chunk("fmt ", pcmFormat()),
		chunk("LIST", []byte("metadata")),
		chunk("data", audio),
	)
	writeFile(t, path, content)

	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Duration() != 100*time.Millisecond || file.Samples() != 1600 || file.DataSize() != 3200 {
		t.Fatalf("métadonnées = duration %v, samples %d, bytes %d", file.Duration(), file.Samples(), file.DataSize())
	}
	if got := readAudio(t, file); !bytes.Equal(got, audio) {
		t.Fatal("les données audio diffèrent")
	}
}

func TestOpenPinsValidatedRIFFAndExcludesPhysicalSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.wav")
	validated := riff(chunk("fmt ", pcmFormat()), chunk("data", samples(16)))
	writeFile(t, path, append(append([]byte(nil), validated...), []byte("CANARY-SUFFIX")...))

	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	replaced := filepath.Join(dir, "validated.wav")
	if err := os.Rename(path, replaced); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("replacement must not be read"))

	reader, handle, err := file.OpenRIFF()
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := handle.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("lecture RIFF: read=%v close=%v", readErr, closeErr)
	}
	if !bytes.Equal(got, validated) {
		t.Fatalf("RIFF lu=%d octets, attendu=%d", len(got), len(validated))
	}
}

func TestOpenSnapshotsBytesBeforeInPlaceMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.wav")
	original := riff(chunk("fmt ", pcmFormat()), chunk("data", samples(16)))
	writeFile(t, path, original)
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	mutated := append([]byte(nil), original...)
	copy(mutated[len(mutated)-16:], bytes.Repeat([]byte{0xff}, 16))
	handle, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := handle.WriteAt(mutated, 0)
	closeErr := handle.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("mutation source: write=%v close=%v", writeErr, closeErr)
	}

	reader, snapshot, err := file.OpenRIFF()
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	if _, err := snapshot.WriteAt([]byte{'X'}, 0); err == nil {
		t.Fatal("snapshot WAV modifiable malgré les seals")
	}
	closeErr = snapshot.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("lecture snapshot: read=%v close=%v", readErr, closeErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("une mutation in-place a modifié le snapshot validé")
	}
}

func TestOpenFileTypePolicy(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.wav")
	writeFile(t, regular, riff(chunk("fmt ", pcmFormat()), chunk("data", samples(1))))

	parentLink := filepath.Join(dir, "parent-link")
	if err := os.Symlink(dir, parentLink); err != nil {
		t.Fatal(err)
	}
	file, err := Open(filepath.Join(parentLink, "regular.wav"))
	if err != nil {
		t.Fatalf("symlink de répertoire parent refusé: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	finalLink := filepath.Join(dir, "final-link.wav")
	if err := os.Symlink(regular, finalLink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "audio.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"final symlink": finalLink,
		"fifo":          fifo,
		"device":        "/dev/null",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(path); err == nil || errs.Classify(err) != errs.AudioFailed {
				t.Fatalf("Open(%q) error=%v", path, err)
			}
		})
	}
}

func TestOpenRejectsCorruptionAndWrongFormats(t *testing.T) {
	tests := map[string][]byte{
		"short":           []byte("RIFF"),
		"not wave":        []byte("RIFF\x04\x00\x00\x00NOPE"),
		"missing fmt":     riff(chunk("data", nil)),
		"data before fmt": riff(chunk("data", samples(1)), chunk("fmt ", pcmFormat())),
		"missing data":    riff(chunk("fmt ", pcmFormat())),
		"odd data":        riff(chunk("fmt ", pcmFormat()), chunk("data", []byte{1})),
		"truncated":       append(riff(chunk("fmt ", pcmFormat()), chunk("data", samples(2))), 0),
	}
	wrong := pcmFormat()
	binary.LittleEndian.PutUint32(wrong[4:8], 48000)
	tests["wrong rate"] = riff(chunk("fmt ", wrong), chunk("data", nil))

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.wav")
			if name == "truncated" {
				binary.LittleEndian.PutUint32(content[4:8], binary.LittleEndian.Uint32(content[4:8])+10)
			}
			writeFile(t, path, content)
			_, err := Open(path)
			if err == nil || errs.Classify(err) != errs.AudioFailed {
				t.Fatalf("Open() error = %v", err)
			}
		})
	}
}

func TestStreamingReadersRejectDataBeforeFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-growing.wav")
	writeFile(t, path, riff(chunk("data", samples(1)), chunk("fmt ", pcmFormat())))

	ready, err := HasAudioData(path)
	if err == nil || ready || errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("HasAudioData() ready=%v error=%v", ready, err)
	}
	snapshot, ok, err := Snapshot(path)
	if err == nil || ok || snapshot.Path() != "" || errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("Snapshot() file=%#v ok=%v error=%v", snapshot, ok, err)
	}
	source, err := OpenGrowing(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshot, ok, err = SnapshotOpen(source, path)
	if err == nil || ok || snapshot.Path() != "" || errs.Classify(err) != errs.AudioFailed {
		t.Fatalf("SnapshotOpen() file=%#v ok=%v error=%v", snapshot, ok, err)
	}
}

func TestHasAudioDataForUnfinishedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.wav")
	header := canonicalHeader(0)
	writeFile(t, path, header)
	ready, err := HasAudioData(path)
	if err != nil || ready {
		t.Fatalf("header only = %v, %v", ready, err)
	}
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
	ready, err = HasAudioData(path)
	if err != nil || !ready {
		t.Fatalf("sample present = %v, %v", ready, err)
	}
}

func TestGrowingSnapshotUsesOnlyCompletePhysicalSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.wav")
	if err := CreateGrowing(path); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err := Snapshot(path)
	if err != nil || !ok || snapshot.Samples() != 0 {
		t.Fatalf("snapshot initial=%#v ok=%v error=%v", snapshot, ok, err)
	}
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, ok, err = Snapshot(path)
	if err != nil || !ok || snapshot.Samples() != 1 || !bytes.Equal(readAudio(t, snapshot), []byte{1, 2}) {
		t.Fatalf("snapshot partiel: samples=%d ok=%v error=%v", snapshot.Samples(), ok, err)
	}
	handle, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := FinalizeGrowing(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.Samples() != 2 || !bytes.Equal(readAudio(t, final), []byte{1, 2, 3, 4}) {
		t.Fatalf("WAV final: samples=%d data=%v", final.Samples(), readAudio(t, final))
	}
}

func TestFinalizeGrowingReturnsSnapshotOfStableDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.wav")
	if err := CreateGrowing(path); err != nil {
		t.Fatal(err)
	}
	want := samples(32)
	if err := AppendPCM(path, want); err != nil {
		t.Fatal(err)
	}
	final, err := FinalizeGrowing(path)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	handle, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := handle.WriteAt(bytes.Repeat([]byte{0xff}, len(want)), headerSize)
	closeErr := handle.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("mutation finale: write=%v close=%v", writeErr, closeErr)
	}
	if got := readAudio(t, final); !bytes.Equal(got, want) {
		t.Fatal("le WAV final dépend encore du chemin mutable")
	}
}

func TestSliceCopiesAnExactSampleRange(t *testing.T) {
	file, err := Open(makeWAV(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	part, err := file.Slice(t.TempDir(), 123, 456)
	if err != nil {
		t.Fatal(err)
	}
	want := readAudio(t, file)[123*BlockAlign : (123+456)*BlockAlign]
	if part.Samples() != 456 || !bytes.Equal(readAudio(t, part), want) {
		t.Fatal("Slice a modifié la plage PCM")
	}
}

func TestSplitRoundTripAndMinimumTail(t *testing.T) {
	source := makeWAV(t, 25*time.Second)
	file, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := file.Split(t.TempDir(), 10*time.Second, 6*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Duration() != 10*time.Second || parts[1].Duration() != 15*time.Second {
		t.Fatalf("durées = %v", durations(parts))
	}
	var joined bytes.Buffer
	for _, part := range parts {
		joined.Write(readAudio(t, part))
	}
	if want := readAudio(t, file); !bytes.Equal(joined.Bytes(), want) {
		t.Fatal("la re-concaténation diffère de l'original")
	}
}

func TestSplitShortPathDoesNotCopy(t *testing.T) {
	source := makeWAV(t, time.Second)
	file, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	parts, err := file.Split(output, 2*time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Path() != source {
		t.Fatalf("Split() = %#v", parts)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("fichiers créés sur le chemin court: %v", entries)
	}
}

func TestSplitValidatesDurations(t *testing.T) {
	file, err := Open(makeWAV(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		d, tail time.Duration
	}{{0, 0}, {time.Nanosecond, 0}, {time.Second, -1}} {
		if _, err := file.Split(t.TempDir(), test.d, test.tail); err == nil {
			t.Fatalf("Split(%v, %v) accepté", test.d, test.tail)
		}
	}
}

func makeWAV(t *testing.T, duration time.Duration) string {
	t.Helper()
	count := int64(duration) * SampleRate / int64(time.Second)
	path := filepath.Join(t.TempDir(), "fixture.wav")
	writeFile(t, path, riff(chunk("fmt ", pcmFormat()), chunk("data", samples(count))))
	return path
}

func samples(count int64) []byte {
	data := make([]byte, count*BlockAlign)
	for i := int64(0); i < count; i++ {
		binary.LittleEndian.PutUint16(data[i*2:i*2+2], uint16(int16(i%32767)))
	}
	return data
}

func pcmFormat() []byte {
	format := make([]byte, 16)
	binary.LittleEndian.PutUint16(format[0:2], 1)
	binary.LittleEndian.PutUint16(format[2:4], Channels)
	binary.LittleEndian.PutUint32(format[4:8], SampleRate)
	binary.LittleEndian.PutUint32(format[8:12], ByteRate)
	binary.LittleEndian.PutUint16(format[12:14], BlockAlign)
	binary.LittleEndian.PutUint16(format[14:16], BitsPerSample)
	return format
}

func chunk(id string, payload []byte) []byte {
	result := make([]byte, 8+len(payload)+len(payload)%2)
	copy(result[0:4], id)
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(payload)))
	copy(result[8:], payload)
	return result
}

func riff(chunks ...[]byte) []byte {
	size := 4
	for _, value := range chunks {
		size += len(value)
	}
	result := make([]byte, 8, size+8)
	copy(result[0:4], "RIFF")
	binary.LittleEndian.PutUint32(result[4:8], uint32(size))
	result = append(result, []byte("WAVE")...)
	for _, value := range chunks {
		result = append(result, value...)
	}
	return result
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAudio(t *testing.T, file File) []byte {
	t.Helper()
	reader, handle, err := file.OpenData()
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCloseInvalidatesAllFileCopies(t *testing.T) {
	file, err := Open(makeWAV(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	copy := file
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, handle, err := copy.OpenRIFF(); err == nil {
		handle.Close()
		t.Fatal("une copie fermée expose encore la source")
	}
}

func durations(files []File) []time.Duration {
	result := make([]time.Duration, len(files))
	for i := range files {
		result[i] = files[i].Duration()
	}
	return result
}

func TestOpenMissingFileWrapsCause(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "missing.wav"))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open() error = %v", err)
	}
}
