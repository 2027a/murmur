package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/stt"
)

// TranscribeFile validates and transcribes a canonical WAV without taking
// ownership of the source file.
func TranscribeFile(ctx context.Context, path string, chunkDuration time.Duration, transcriber *stt.Chain, progress stt.Progress) (result stt.MultiResult, err error) {
	file, err := wav.Open(path)
	if err != nil {
		return stt.MultiResult{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if transcriber == nil {
		return stt.MultiResult{}, errs.New(errs.Internal, "stt", "transcriber absent")
	}
	if chunkDuration > 0 {
		count, countErr := file.SegmentCount(chunkDuration, time.Second)
		if countErr != nil {
			return stt.MultiResult{}, countErr
		}
		if countErr := transcriber.ValidateSegmentCount(count); countErr != nil {
			return stt.MultiResult{}, countErr
		}
	}

	outputDir := ""
	if chunkDuration > 0 && file.Duration() > chunkDuration {
		outputDir, err = os.MkdirTemp("", "murmur-transcribe-")
		if err != nil {
			return stt.MultiResult{}, errs.Wrap(errs.AudioFailed, "audio", "création du répertoire de découpage impossible", err)
		}
		defer os.RemoveAll(outputDir)
	}
	segments, err := audio.Chunk(file, audio.ChunkOptions{
		Duration: chunkDuration, MinTail: time.Second, OutputDir: outputDir,
	})
	if err != nil {
		return stt.MultiResult{}, err
	}

	requests := make(chan stt.Segment, len(segments))
	for _, segment := range segments {
		requests <- stt.Segment{Index: segment.Index, Request: stt.Request{
			Audio: segment.File, Filename: filepath.Base(segment.File.Path()),
		}}
	}
	close(requests)
	result, err = transcriber.TranscribeStream(ctx, requests, progress)
	cleanupErr := audio.RemoveSegments(segments)
	if err != nil {
		return stt.MultiResult{}, errors.Join(err, cleanupErr)
	}
	if cleanupErr != nil {
		result.Warnings = append(result.Warnings, stt.Warning{
			Class: errs.AudioFailed, Provider: result.Provider, Component: "audio", Message: "nettoyage audio dégradé",
		})
	}
	return result, nil
}
