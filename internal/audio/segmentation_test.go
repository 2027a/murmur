package audio

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
)

func TestEarlySegmentsPreserveEverySampleAtStop(t *testing.T) {
	for _, duration := range []time.Duration{100 * time.Millisecond, 5 * time.Second, 5500 * time.Millisecond, 6 * time.Second, 6100 * time.Millisecond, 10 * time.Second, 10100 * time.Millisecond, 12 * time.Second} {
		for _, pause := range []time.Duration{0, 500 * time.Millisecond} {
			t.Run(duration.String()+"/pause="+pause.String(), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "growing.wav")
				if err := wav.CreateGrowing(path); err != nil {
					t.Fatal(err)
				}
				s, err := NewGrowingSegmenter(path, dir, LiveOptions{SegmentDuration: 5 * time.Second, MinimumTail: time.Second, PauseWindow: pause})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				pcm := make([]byte, liveDurationSamples(duration)*wav.BlockAlign)
				for i := 0; i < len(pcm); i += 2 {
					// Audible ramp with a 300 ms pause just before 5 s.
					sample := int16(1000 + (i/2)%10000)
					if i >= 4600*wav.ByteRate/1000 && i < 4900*wav.ByteRate/1000 {
						sample = 0
					}
					binary.LittleEndian.PutUint16(pcm[i:], uint16(sample))
				}
				var emitted []LiveSegment
				step := wav.ByteRate / 10
				for start := 0; start < len(pcm); start += step {
					end := min(start+step, len(pcm))
					if err := wav.AppendPCM(path, pcm[start:end]); err != nil {
						t.Fatal(err)
					}
					parts, err := s.Poll()
					if err != nil {
						t.Fatal(err)
					}
					if len(parts) > 1 {
						t.Fatal("poll materialized a backlog")
					}
					emitted = append(emitted, parts...)
					if end == 6*wav.ByteRate && len(emitted) != 1 {
						t.Fatal("first segment not emitted at six seconds")
					}
				}
				final, err := wav.FinalizeGrowing(path)
				if err != nil {
					t.Fatal(err)
				}
				defer final.Close()
				for {
					part, err := s.NextFinal(final)
					if err != nil {
						t.Fatal(err)
					}
					if part == nil {
						break
					}
					emitted = append(emitted, *part)
				}
				var joined []byte
				for i, part := range emitted {
					if part.Index != i {
						t.Fatalf("index=%d want=%d", part.Index, i)
					}
					if len(emitted) > 1 && part.File.Duration() < time.Second {
						t.Fatalf("tiny tail: %v", part.File.Duration())
					}
					joined = append(joined, readWAVFileData(t, part.File)...)
				}
				if !bytes.Equal(joined, pcm) {
					t.Fatal("PCM lost or duplicated")
				}
				if duration >= 6*time.Second && pause > 0 && emitted[0].File.Duration() != 4800*time.Millisecond {
					t.Fatalf("cut=%v", emitted[0].File.Duration())
				}
			})
		}
	}
}

func TestPauseChunkLimitPrecedesSnapshots(t *testing.T) {
	file, err := wav.Open(audioFixture(t, 12*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	dir := t.TempDir()
	_, err = Chunk(file, ChunkOptions{
		Duration: 5 * time.Second, MinTail: time.Second, PauseWindow: 500 * time.Millisecond, OutputDir: dir,
		ValidateCount: func(count int) error {
			if count > 2 {
				return errs.New(errs.ServerError, "stt", "limit")
			}
			return nil
		},
	})
	if errs.Classify(err) != errs.ServerError {
		t.Fatalf("error=%v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("premature snapshots: %v %v", files, err)
	}
}

func TestLiveBacklogIsMaterializedOneSegmentAtATime(t *testing.T) {
	path := audioFixture(t, 30*time.Second)
	dir := t.TempDir()
	s, err := NewGrowingSegmenter(path, dir, LiveOptions{SegmentDuration: 5 * time.Second, MinimumTail: time.Second, MaxSegments: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 2 {
		parts, err := s.Poll()
		if err != nil || len(parts) != 1 {
			t.Fatalf("parts=%d err=%v", len(parts), err)
		}
	}
	if _, err := s.Poll(); errs.Classify(err) != errs.ServerError {
		t.Fatalf("limit error=%v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("snapshots=%d err=%v", len(files), err)
	}
}

func TestOverlapUsesTheSamePCMInBatchAndLive(t *testing.T) {
	path := audioFixture(t, 12*time.Second)
	file, err := wav.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := ChunkOptions{Duration: 5 * time.Second, MinTail: time.Second, PauseWindow: 500 * time.Millisecond, Overlap: 500 * time.Millisecond, OutputDir: t.TempDir()}
	batch, err := Chunk(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer RemoveSegments(batch)
	s, err := NewGrowingSegmenter(path, t.TempDir(), LiveOptions{SegmentDuration: options.Duration, MinimumTail: options.MinTail, PauseWindow: options.PauseWindow, Overlap: options.Overlap})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var live []LiveSegment
	for {
		parts, err := s.Poll()
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) == 0 {
			break
		}
		live = append(live, parts...)
	}
	final, err := s.Finalize(file)
	if err != nil {
		t.Fatal(err)
	}
	live = append(live, final...)
	if len(batch) != len(live) {
		t.Fatalf("batch=%d live=%d", len(batch), len(live))
	}
	var pcm []byte
	for i, part := range batch {
		data := readWAVFileData(t, part.File)
		if part.Overlap != (i > 0) || part.Overlap != live[i].Overlap || !bytes.Equal(data, readWAVFileData(t, live[i].File)) {
			t.Fatalf("segment %d differs", i)
		}
		if i > 0 {
			overlap := wav.ByteRate / 2
			if !bytes.Equal(data[:overlap], pcm[len(pcm)-overlap:]) {
				t.Fatal("overlap PCM differs")
			}
			data = data[overlap:]
		}
		pcm = append(pcm, data...)
	}
	if !bytes.Equal(pcm, readWAVFileData(t, file)) {
		t.Fatal("overlap reconstruction lost PCM")
	}
}
