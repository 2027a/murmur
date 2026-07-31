package pwrecord

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio"
	"git.2027a.net/mathieu/murmur/internal/errs"
)

type liveStopRequest struct {
	ctx    context.Context
	result chan error
}

type liveRecording struct {
	recording audio.Recording
	segmenter *audio.GrowingSegmenter
	tempDir   string

	ctx    context.Context
	cancel context.CancelFunc
	ready  chan error
	parts  chan audio.LiveSegment
	errors chan error
	stop   chan liveStopRequest
	done   chan struct{}

	mu       sync.Mutex
	final    audio.WavFile
	runErr   error
	cleanup  sync.Once
	kept     string
	cleanErr error
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
	segmenter, err := audio.NewGrowingSegmenter(concrete.path, tempDir, live)
	if err != nil {
		cancel()
		_ = child.Abort()
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	recording := &liveRecording{
		recording: child, segmenter: segmenter, tempDir: tempDir,
		ctx: liveCtx, cancel: cancel, ready: make(chan error, 1),
		parts: make(chan audio.LiveSegment, 2), errors: make(chan error, 1),
		stop: make(chan liveStopRequest), done: make(chan struct{}),
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
		err := recordingEnded(r.recording)
		r.publishReady(err)
		_ = r.recording.Abort()
		r.finish(err)
		return
	case <-r.ctx.Done():
		r.publishReady(r.ctx.Err())
		_ = r.recording.Abort()
		r.finish(r.ctx.Err())
		return
	}
	r.publishReady(nil)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.recording.Done():
			err := recordingEnded(r.recording)
			_ = r.recording.Abort()
			r.finish(err)
			return
		case <-r.ctx.Done():
			_ = r.recording.Abort()
			r.finish(r.ctx.Err())
			return
		case request := <-r.stop:
			err := r.finalize(request.ctx)
			r.finish(err)
			request.result <- err
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

func (r *liveRecording) finalize(ctx context.Context) error {
	file, err := r.recording.Stop(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.final = file
	r.mu.Unlock()
	segments, err := r.segmenter.Finalize(file.File)
	if err != nil {
		return err
	}
	return r.emit(segments)
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
	request := liveStopRequest{ctx: ctx, result: make(chan error, 1)}
	select {
	case r.stop <- request:
	case <-r.done:
		return r.result()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.result:
		return err
	case <-ctx.Done():
		r.cancel()
		<-r.done
		return ctx.Err()
	}
}

func (r *liveRecording) Abort() error {
	r.cancel()
	<-r.done
	r.cleanup.Do(func() { r.cleanErr = os.RemoveAll(r.tempDir) })
	if err := r.result(); err != nil && !errors.Is(err, context.Canceled) {
		return errors.Join(err, r.cleanErr)
	}
	return r.cleanErr
}

func (r *liveRecording) Release() (string, error) {
	r.cleanup.Do(func() {
		r.mu.Lock()
		final := r.final
		r.mu.Unlock()
		if final.Path() == "" {
			r.cleanErr = os.RemoveAll(r.tempDir)
			return
		}
		finalCleanupResult, err := final.Release()
		r.kept = finalCleanupResult
		r.cleanErr = errors.Join(err, os.RemoveAll(r.tempDir))
	})
	return r.kept, r.cleanErr
}

func (r *liveRecording) result() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

func recordingEnded(recording audio.Recording) error {
	if err := recording.Err(); err != nil {
		return err
	}
	return errs.New(errs.AudioFailed, "audio/pw-record", "pw-record s'est arrêté pendant la capture live")
}

var _ audio.LiveRecorder = (*Recorder)(nil)
var _ audio.LiveRecording = (*liveRecording)(nil)
