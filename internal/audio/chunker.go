package audio

import (
	"errors"
	"os"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
)

type ChunkOptions struct {
	Duration      time.Duration
	MinTail       time.Duration
	OutputDir     string
	PauseWindow   time.Duration
	Overlap       time.Duration
	ValidateCount func(int) error
}

type Segment struct {
	Index     int
	File      wav.File
	Temporary bool
	Overlap   bool
}

func Chunk(file wav.File, options ChunkOptions) ([]Segment, error) {
	if options.Duration == 0 || file.Duration() <= options.Duration {
		return []Segment{{Index: 0, File: file}}, nil
	}
	if options.PauseWindow > 0 || options.Overlap > 0 {
		return chunkOnPauses(file, options)
	}
	parts, err := file.Split(options.OutputDir, options.Duration, options.MinTail)
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, len(parts))
	for i, part := range parts {
		segments[i] = Segment{Index: i, File: part, Temporary: part.Path() != file.Path()}
	}
	return segments, nil
}

func chunkOnPauses(file wav.File, options ChunkOptions) ([]Segment, error) {
	if err := ValidateLiveOptions(LiveOptions{SegmentDuration: options.Duration, MinimumTail: options.MinTail, PauseWindow: options.PauseWindow, Overlap: options.Overlap}); err != nil {
		return nil, err
	}
	// Plan and enforce the exact count before creating any snapshots.
	var ends []int64
	maximum, tail := liveDurationSamples(options.Duration), liveDurationSamples(options.MinTail)
	for cursor := int64(0); cursor < file.Samples(); {
		if options.ValidateCount != nil {
			if err := options.ValidateCount(len(ends) + 1); err != nil {
				return nil, err
			}
		}
		end := file.Samples()
		if end-cursor > maximum && end-cursor >= maximum+tail {
			var err error
			end, err = file.PauseCut(cursor+maximum, options.PauseWindow)
			if err != nil {
				return nil, err
			}
		}
		ends = append(ends, end)
		cursor = end
	}
	var segments []Segment
	var cursor int64
	for i, end := range ends {
		start := max(int64(0), cursor-liveDurationSamples(options.Overlap))
		part, err := file.Slice(options.OutputDir, start, end-start)
		if err != nil {
			return nil, errors.Join(err, RemoveSegments(segments))
		}
		segments = append(segments, Segment{Index: i, File: part, Temporary: true, Overlap: start < cursor})
		cursor = end
	}
	return segments, nil
}

func RemoveSegments(segments []Segment) error {
	var result error
	for _, segment := range segments {
		if segment.Temporary {
			result = errors.Join(result, segment.File.Close(), os.Remove(segment.File.Path()))
		}
	}
	return result
}
