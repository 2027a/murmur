package wav

import (
	"encoding/binary"
	"math"
	"os"
)

// Level contains only bounded acoustic metrics, never audio samples. UpdatedMS
// is set by the recorder only when fresh PCM has actually been observed.
type Level struct {
	RMS       float64 `json:"rms"`
	Peak      float64 `json:"peak"`
	Clipping  bool    `json:"clipping"`
	UpdatedMS int64   `json:"updated_ms"`
}

// MeasureGrowing reads at most the most recent 100 ms of new PCM. SnapshotOpen
// inspects the existing descriptor; it does not copy the recording. A slow UI
// never causes a backlog of audio to be read or retained.
func MeasureGrowing(source *os.File, previous int64) (Level, int64, error) {
	f, ready, err := SnapshotOpen(source, source.Name())
	if err != nil || !ready {
		return Level{}, previous, err
	}
	defer f.Close()
	end := f.DataSize()
	if end <= previous {
		return Level{}, previous, nil
	}
	start := max(previous, end-ByteRate/10)
	var buf [ByteRate / 10]byte
	n, err := source.ReadAt(buf[:end-start], f.dataOffset+start)
	if err != nil {
		return Level{}, previous, err
	}
	return measurePCM(buf[:n]), end, nil
}

func measurePCM(pcm []byte) Level {
	var sum, peak float64
	var clipped bool
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(pcm[i : i+2]))
		value := math.Abs(float64(sample) / 32768)
		sum += value * value
		peak = max(peak, value)
		clipped = clipped || sample == 32767 || sample == -32768
	}
	if len(pcm) < 2 {
		return Level{}
	}
	return Level{RMS: math.Sqrt(sum / float64(len(pcm)/2)), Peak: peak, Clipping: clipped}
}
