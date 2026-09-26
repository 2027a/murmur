package audio

import (
	"errors"
	"os"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
)

type ChunkOptions struct {
	Duration  time.Duration
	MinTail   time.Duration
	OutputDir string
}

type Segment struct {
	Index     int
	File      wav.File
	Temporary bool
}

func Chunk(file wav.File, options ChunkOptions) ([]Segment, error) {
	if options.Duration == 0 || file.Duration() <= options.Duration {
		return []Segment{{Index: 0, File: file}}, nil
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

func RemoveSegments(segments []Segment) error {
	var result error
	for _, segment := range segments {
		if segment.Temporary {
			result = errors.Join(result, segment.File.Close(), os.Remove(segment.File.Path()))
		}
	}
	return result
}
