package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/stt"
)

// TranscribeFile validates and transcribes a canonical WAV without taking
// ownership of the source file.
func TranscribeFile(ctx context.Context, path string, chunkDuration time.Duration, transcriber *stt.Chain, progress stt.Progress) (result stt.MultiResult, err error) {
	return TranscribeFileWithOptions(ctx, path, audio.ChunkOptions{Duration: chunkDuration, MinTail: time.Second}, transcriber, progress)
}

func TranscribeFileWithOptions(ctx context.Context, path string, options audio.ChunkOptions, transcriber *stt.Chain, progress stt.Progress) (result stt.MultiResult, err error) {
	chunkDuration := options.Duration
	file, err := wav.Open(path)
	if err != nil {
		return stt.MultiResult{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if transcriber == nil {
		return stt.MultiResult{}, errs.New(errs.Internal, "stt", "transcriber absent")
	}
	if chunkDuration > 0 {
		count, countErr := file.SegmentCount(chunkDuration, options.MinTail)
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
	options.OutputDir = outputDir
	options.ValidateCount = transcriber.ValidateSegmentCount
	segments, err := audio.Chunk(file, options)
	if err != nil {
		return stt.MultiResult{}, err
	}

	requests := make([]stt.Segment, len(segments))
	for i, segment := range segments {
		requests[i] = stt.Segment{Index: segment.Index, Overlap: segment.Overlap, Request: stt.Request{
			Audio: segment.File, Filename: filepath.Base(segment.File.Path()),
		}}
	}
	result, err = transcriber.TranscribeSegments(ctx, requests, progress)
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
