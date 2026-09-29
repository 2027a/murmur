package wav

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"
)

func TestPCMLevelsAndClipping(t *testing.T) {
	for _, test := range []struct {
		name      string
		samples   []int16
		rms, peak float64
		clip      bool
	}{
		{"silence", []int16{0, 0}, 0, 0, false},
		{"half scale", []int16{-16384, 16384}, .5, .5, false},
		{"RMS not average", []int16{0, 16384}, math.Sqrt(.125), .5, false},
		{"negative clipping", []int16{-32768}, 1, 1, true},
		{"positive clipping", []int16{32767}, 32767.0 / 32768, 32767.0 / 32768, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pcm := make([]byte, len(test.samples)*2)
			for i, sample := range test.samples {
				binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
			}
			got := measurePCM(pcm)
			if math.Abs(got.RMS-test.rms) > 1e-9 || got.Peak != test.peak || got.Clipping != test.clip {
				t.Fatalf("level=%+v", got)
			}
		})
	}
}

func TestGrowingLevelsReadOnlyFreshBoundedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.wav")
	if err := CreateGrowing(path); err != nil {
		t.Fatal(err)
	}
	source, err := OpenGrowing(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	pcm := make([]byte, ByteRate) // Simulate a slow consumer: one second behind.
	for i := 0; i < len(pcm)-ByteRate/10; i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 32767)
	}
	if err := AppendPCM(path, pcm); err != nil {
		t.Fatal(err)
	}
	level, offset, err := MeasureGrowing(source, 0)
	if err != nil || offset != ByteRate || level.RMS != 0 || level.Clipping {
		t.Fatalf("stale prefix measured: %+v %d %v", level, offset, err)
	}
	_, next, err := MeasureGrowing(source, offset)
	if err != nil || next != offset {
		t.Fatalf("replayed audio: %d %v", next, err)
	}
	if err := AppendPCM(path, []byte{0, 64}); err != nil {
		t.Fatal(err)
	}
	level, next, err = MeasureGrowing(source, offset)
	if err != nil || level.RMS != .5 || next != offset+2 {
		t.Fatalf("new sample=%+v %d %v", level, next, err)
	}
}
