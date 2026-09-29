package wav

import (
	"encoding/binary"
	"io"
	"time"
)

// PauseCut chooses the latest low-energy pause in the window preceding target.
// It never waits beyond target, removes no PCM and falls back to a fixed cut.
// This is an energy heuristic, not speech recognition or a silence filter.
func (f File) PauseCut(target int64, window time.Duration) (int64, error) {
	if window == 0 {
		return target, nil
	}
	const frame = SampleRate / 100 // 10 ms
	const quietFrames = 20         // 200 ms
	const threshold = 327          // approximately -40 dBFS RMS
	start := max(int64(0), target-durationSamples(window))
	reader, handle, err := f.OpenData()
	if err != nil {
		return 0, err
	}
	defer handle.Close()
	var pcm [frame * BlockAlign]byte
	quiet := 0
	cut := target
	found := false
	for end := target; end-int64(frame) >= start; end -= frame {
		if _, err := reader.ReadAt(pcm[:], (end-frame)*BlockAlign); err != nil {
			return 0, audioError("lecture de la fenêtre de pause impossible", err)
		}
		var energy int64
		for i := 0; i < len(pcm); i += BlockAlign {
			sample := int64(int16(binary.LittleEndian.Uint16(pcm[i:])))
			energy += sample * sample
		}
		if energy <= int64(frame*threshold*threshold) {
			quiet++
			if quiet == quietFrames {
				// Leave half the detected pause on each side of the boundary.
				cut = end + (quietFrames/2-1)*frame
				found = true
				break
			}
		} else {
			quiet = 0
		}
	}
	if found && cut <= 0 {
		return 0, audioError("frontière de pause invalide", io.ErrUnexpectedEOF)
	}
	return cut, nil
}
