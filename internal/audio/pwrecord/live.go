package pwrecord

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
)

type liveCaptureResult struct {
	file audio.WavFile
	err  error
}

type liveRecording struct {
	recording audio.Recording
	segmenter *audio.GrowingSegmenter
	tempDir   string
	path      string
	live      audio.LiveOptions

	ctx     context.Context
	cancel  context.CancelFunc
	ready   chan error
	parts   chan audio.LiveSegment
	errors  chan error
	stop    chan struct{}
	stopped chan liveCaptureResult
	done    chan struct{}

	mu         sync.Mutex
	final      audio.WavFile
	runErr     error
	actionOnce sync.Once
	cleanup    sync.Once
	kept       string
	cleanErr   error
}

func (r *Recorder) StartLive(ctx context.Context, opts audio.RecordOptions, live audio.LiveOptions) (audio.LiveRecording, error) {
	if err := audio.ValidateLiveOptions(live); err != nil {
		return nil, err
	}
	normalized, err := audio.NormalizeRecordOptions(opts)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp(normalized.Directory, "murmur-audio-live-")
	if err != nil {
		return nil, errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible de créer le répertoire de capture live", err)
	}
	childOptions := normalized
	childOptions.Directory = tempDir
	liveCtx, cancel := context.WithCancel(ctx)
	child, err := r.Start(liveCtx, childOptions)
	if err != nil {
		cancel()
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	concrete, ok := child.(*recording)
	if !ok {
		cancel()
		_ = child.Abort()
		_ = os.RemoveAll(tempDir)
		return nil, errs.New(errs.Internal, "audio/pw-record", "capture pw-record live incohérente")
	}
	recording := &liveRecording{
		recording: child, tempDir: tempDir, path: concrete.path, live: live,
		ctx: liveCtx, cancel: cancel, ready: make(chan error, 1),
		parts: make(chan audio.LiveSegment, 2), errors: make(chan error, 1),
		stop: make(chan struct{}), stopped: make(chan liveCaptureResult), done: make(chan struct{}),
	}
	go recording.run(normalized.ReadinessInterval)
	return recording, nil
}

func (r *liveRecording) Ready() <-chan error                { return r.ready }
func (r *liveRecording) Done() <-chan struct{}              { return r.done }
func (r *liveRecording) Err() error                         { return r.result() }
func (r *liveRecording) Segments() <-chan audio.LiveSegment { return r.parts }
func (r *liveRecording) Errors() <-chan error               { return r.errors }

func (r *liveRecording) run(interval time.Duration) {
	defer r.cancel()
	defer close(r.done)
	defer close(r.parts)
	defer close(r.errors)
	select {
	case err := <-r.recording.Ready():
		if err != nil {
			r.publishReady(err)
			_ = r.recording.Abort()
			r.finish(err)
			return
		}
	case <-r.recording.Done():
		err := r.endedOrStopped()
		r.publishReady(err)
		if !r.stopRequested() {
			_ = r.recording.Abort()
		}
		r.finish(err)
		return
	case result := <-r.stopped:
		err := r.finalize(result)
		r.publishReady(err)
		r.finish(err)
		return
	case <-r.ctx.Done():
		r.publishReady(r.ctx.Err())
		_ = r.recording.Abort()
		r.finish(r.ctx.Err())
		return
	}
	if err := r.initSegmenter(); err != nil {
		r.publishReady(err)
		_ = r.recording.Abort()
		r.finish(err)
		return
	}
	r.publishReady(nil)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.recording.Done():
			err := r.endedOrStopped()
			if !r.stopRequested() {
				_ = r.recording.Abort()
			}
			r.finish(err)
			return
		case result := <-r.stopped:
			r.finish(r.finalize(result))
			return
		case <-r.ctx.Done():
			_ = r.recording.Abort()
			r.finish(r.ctx.Err())
			return
		case <-ticker.C:
			segments, err := r.segmenter.Poll()
			if err != nil {
				_ = r.recording.Abort()
				r.finish(err)
				return
			}
			if err := r.emit(segments); err != nil {
				_ = r.recording.Abort()
				r.finish(err)
				return
			}
		}
	}
}

func (r *liveRecording) initSegmenter() error {
	if r.segmenter != nil {
		return nil
	}
	source, err := wav.OpenGrowing(r.path)
	if err != nil {
		return err
	}
	segmenter, err := audio.NewGrowingSegmenterFromFile(source, r.path, r.tempDir, r.live)
	if err != nil {
		_ = source.Close()
		return err
	}
	r.segmenter = segmenter
	return nil
}

func (r *liveRecording) finalize(result liveCaptureResult) error {
	if result.err != nil {
		return result.err
	}
	if err := r.initSegmenter(); err != nil {
		return errors.Join(err, result.file.File.Close())
	}
	r.mu.Lock()
	r.final = result.file
	r.mu.Unlock()
	for {
		segment, err := r.segmenter.NextFinal(result.file.File)
		if err != nil || segment == nil {
			return err
		}
		if err := r.emit([]audio.LiveSegment{*segment}); err != nil {
			return err
		}
	}
}

func (r *liveRecording) emit(segments []audio.LiveSegment) error {
	for _, segment := range segments {
		select {
		case r.parts <- segment:
		case <-r.ctx.Done():
			return r.ctx.Err()
		}
	}
	return nil
}

func (r *liveRecording) publishReady(err error) {
	if err != nil {
		r.ready <- err
	}
	close(r.ready)
}

func (r *liveRecording) finish(err error) {
	r.mu.Lock()
	r.runErr = err
	r.mu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		r.errors <- err
	}
}

func (r *liveRecording) Stop(ctx context.Context) error {
	select {
	case <-r.done:
		return r.result()
	default:
	}
	owner := false
	r.actionOnce.Do(func() {
		owner = true
		close(r.stop)
		go func() {
			file, err := r.recording.Stop(ctx)
			result := liveCaptureResult{file: file, err: err}
			select {
			case r.stopped <- result:
			case <-r.done:
				if file.Path() != "" {
					_ = file.File.Close()
				}
			}
		}()
	})
	select {
	case <-r.done:
		return r.result()
	case <-ctx.Done():
		if !owner {
			return ctx.Err()
		}
		r.cancel()
		<-r.done
		return ctx.Err()
	}
}

func (r *liveRecording) Abort() error {
	owner := false
	r.actionOnce.Do(func() {
		owner = true
		r.cancel()
	})
	if !owner {
		<-r.done
		return r.result()
	}
	<-r.done
	r.cleanup.Do(func() {
		r.mu.Lock()
		final := r.final
		r.mu.Unlock()
		r.cleanErr = errors.Join(r.closeSegmenter(), final.File.Close(), os.RemoveAll(r.tempDir))
	})
	if err := r.result(); err != nil && !errors.Is(err, context.Canceled) {
		return errors.Join(err, r.cleanErr)
	}
	return r.cleanErr
}

func (r *liveRecording) Discard() error {
	<-r.done
	r.cleanup.Do(func() {
		r.mu.Lock()
		final := r.final
		r.mu.Unlock()
		r.cleanErr = errors.Join(r.closeSegmenter(), final.File.Close(), os.RemoveAll(r.tempDir))
	})
	return r.cleanErr
}

func (r *liveRecording) Release() (string, error) {
	r.cleanup.Do(func() {
		r.mu.Lock()
		final := r.final
		r.mu.Unlock()
		segmentErr := r.closeSegmenter()
		if final.Path() == "" {
			r.cleanErr = errors.Join(segmentErr, os.RemoveAll(r.tempDir))
			return
		}
		finalCleanupResult, err := final.Release()
		r.kept = finalCleanupResult
		r.cleanErr = errors.Join(segmentErr, err, os.RemoveAll(r.tempDir))
	})
	return r.kept, r.cleanErr
}

func (r *liveRecording) closeSegmenter() error {
	if r.segmenter == nil {
		return nil
	}
	return r.segmenter.Close()
}

func (r *liveRecording) result() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

func (r *liveRecording) stopRequested() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *liveRecording) endedOrStopped() error {
	if !r.stopRequested() {
		return recordingEnded(r.recording)
	}
	select {
	case result := <-r.stopped:
		return r.finalize(result)
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func recordingEnded(recording audio.Recording) error {
	if err := recording.Err(); err != nil {
		return err
	}
	return errs.New(errs.AudioFailed, "audio/pw-record", "pw-record s'est arrêté pendant la capture live")
}

var _ audio.LiveRecorder = (*Recorder)(nil)
var _ audio.LiveRecording = (*liveRecording)(nil)
