package audio

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
)

type LiveOptions struct {
	SegmentDuration time.Duration
	MinimumTail     time.Duration
}

type LiveSegment struct {
	Index int
	File  wav.File
}

type LiveRecorder interface {
	StartLive(context.Context, RecordOptions, LiveOptions) (LiveRecording, error)
}

type LiveRecording interface {
	Ready() <-chan error
	Done() <-chan struct{}
	Err() error
	Segments() <-chan LiveSegment
	Errors() <-chan error
	Stop(context.Context) error
	Abort() error
	Release() (string, error)
}

func ValidateLiveOptions(options LiveOptions) error {
	if options.SegmentDuration <= 0 {
		return errs.New(errs.ConfigInvalid, "audio", "la durée des segments live doit être strictement positive")
	}
	if options.MinimumTail < 0 {
		return errs.New(errs.ConfigInvalid, "audio", "la durée minimale de fin live ne peut pas être négative")
	}
	return nil
}

// GrowingSegmenter snapshots a WAV written by another process. It keeps the
// latest full segment so a short final tail can be merged without retranscribing.
type GrowingSegmenter struct {
	path           string
	source         *os.File
	outputDir      string
	segmentSamples int64
	minimumTail    int64
	cursor         int64
	pending        *wav.File
	files          []wav.File
	nextIndex      int
}

func NewGrowingSegmenter(path, outputDir string, options LiveOptions) (*GrowingSegmenter, error) {
	return newGrowingSegmenter(nil, path, outputDir, options)
}

func NewGrowingSegmenterFromFile(source *os.File, path, outputDir string, options LiveOptions) (*GrowingSegmenter, error) {
	if source == nil {
		return nil, errs.New(errs.AudioFailed, "audio", "descripteur de capture live absent")
	}
	return newGrowingSegmenter(source, path, outputDir, options)
}

func newGrowingSegmenter(source *os.File, path, outputDir string, options LiveOptions) (*GrowingSegmenter, error) {
	if err := ValidateLiveOptions(options); err != nil {
		return nil, err
	}
	segmentSamples := liveDurationSamples(options.SegmentDuration)
	if segmentSamples < 1 {
		return nil, errs.New(errs.ConfigInvalid, "audio", "la durée live est inférieure à un échantillon")
	}
	return &GrowingSegmenter{
		path: path, source: source, outputDir: outputDir, segmentSamples: segmentSamples,
		minimumTail: liveDurationSamples(options.MinimumTail),
	}, nil
}

func (s *GrowingSegmenter) Poll() ([]LiveSegment, error) {
	snapshot, ok, err := s.snapshot()
	if err != nil || !ok {
		return nil, err
	}
	defer snapshot.Close()
	var emitted []LiveSegment
	for snapshot.Samples()-s.cursor >= s.segmentSamples {
		part, err := snapshot.Slice(s.outputDir, s.cursor, s.segmentSamples)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, part)
		s.cursor += s.segmentSamples
		if s.pending != nil {
			emitted = append(emitted, s.segment(*s.pending))
		}
		s.pending = &part
	}
	return emitted, nil
}

func (s *GrowingSegmenter) Finalize(final wav.File) ([]LiveSegment, error) {
	if final.Path() != s.path || final.Samples() < s.cursor {
		return nil, errs.New(errs.AudioFailed, "audio", "le WAV final ne correspond pas aux snapshots live")
	}
	var emitted []LiveSegment
	for final.Samples()-s.cursor >= s.segmentSamples {
		part, err := final.Slice(s.outputDir, s.cursor, s.segmentSamples)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, part)
		s.cursor += s.segmentSamples
		if s.pending != nil {
			emitted = append(emitted, s.segment(*s.pending))
		}
		s.pending = &part
	}
	var tail *wav.File
	if remaining := final.Samples() - s.cursor; remaining > 0 {
		part, err := final.Slice(s.outputDir, s.cursor, remaining)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, part)
		tail = &part
	}
	if s.pending != nil && tail != nil && tail.Samples() < s.minimumTail {
		merged, err := wav.Concat(s.outputDir, *s.pending, *tail)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, merged)
		emitted = append(emitted, s.segment(merged))
		return emitted, nil
	}
	if s.pending != nil {
		emitted = append(emitted, s.segment(*s.pending))
	}
	if tail != nil {
		emitted = append(emitted, s.segment(*tail))
	}
	return emitted, nil
}

func (s *GrowingSegmenter) Close() error {
	var result error
	if s.source != nil {
		result = errors.Join(result, s.source.Close())
		s.source = nil
	}
	for _, file := range s.files {
		result = errors.Join(result, file.Close())
	}
	s.files = nil
	return result
}

func (s *GrowingSegmenter) snapshot() (wav.File, bool, error) {
	if s.source != nil {
		return wav.SnapshotOpen(s.source, s.path)
	}
	return wav.Snapshot(s.path)
}

func (s *GrowingSegmenter) segment(file wav.File) LiveSegment {
	segment := LiveSegment{Index: s.nextIndex, File: file}
	s.nextIndex++
	return segment
}

func (f *FakeRecorder) StartLive(ctx context.Context, opts RecordOptions, live LiveOptions) (LiveRecording, error) {
	if f.StartErr != nil {
		return nil, f.StartErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if err := ValidateLiveOptions(live); err != nil {
		return nil, err
	}
	source, err := wav.Open(f.FixturePath)
	if err != nil {
		return nil, err
	}
	reader, handle, err := source.OpenData()
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}
	data, readErr := io.ReadAll(reader)
	closeErr := handle.Close()
	sourceCloseErr := source.Close()
	if readErr != nil || closeErr != nil || sourceCloseErr != nil {
		return nil, errors.Join(readErr, closeErr, sourceCloseErr)
	}
	tempDir, err := os.MkdirTemp(opts.Directory, "murmur-audio-live-")
	if err != nil {
		return nil, errs.Wrap(errs.AudioFailed, "audio/fake", "impossible de créer le répertoire de capture live", err)
	}
	path := filepath.Join(tempDir, "recording.wav")
	if err := wav.CreateGrowing(path); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	segmenter, err := NewGrowingSegmenter(path, tempDir, live)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	stepSamples := f.LiveStepSamples
	if stepSamples < 1 {
		stepSamples = liveDurationSamples(live.SegmentDuration)
	}
	interval := f.LiveStepInterval
	if interval <= 0 {
		interval = time.Millisecond
	}
	liveCtx, cancel := context.WithCancel(ctx)
	r := &fakeLiveRecording{
		path: path, tempDir: tempDir, opts: opts, data: data,
		stepBytes: stepSamples * wav.BlockAlign, interval: interval, segmenter: segmenter,
		ctx: liveCtx, cancel: cancel, ready: make(chan error, 1),
		segments: make(chan LiveSegment, 2), errors: make(chan error, 1),
		stop: make(chan fakeStopRequest), stopping: make(chan struct{}), done: make(chan struct{}),
		failure: f.DoneGate, failureErr: f.DoneErr,
	}
	go r.run(f.ReadyGate, f.ReadyErr)
	return r, nil
}

type fakeStopRequest struct {
	file wav.File
	err  error
}

type fakeLiveRecording struct {
	path       string
	tempDir    string
	opts       RecordOptions
	data       []byte
	written    int64
	stepBytes  int64
	interval   time.Duration
	segmenter  *GrowingSegmenter
	ctx        context.Context
	cancel     context.CancelFunc
	ready      chan error
	segments   chan LiveSegment
	errors     chan error
	stop       chan fakeStopRequest
	stopping   chan struct{}
	done       chan struct{}
	failure    <-chan struct{}
	failureErr error

	mu        sync.Mutex
	runErr    error
	stopOnce  sync.Once
	captureMu sync.Mutex
	cleanup   sync.Once
	release   string
	cleanErr  error
}

func (r *fakeLiveRecording) Ready() <-chan error          { return r.ready }
func (r *fakeLiveRecording) Done() <-chan struct{}        { return r.done }
func (r *fakeLiveRecording) Err() error                   { return r.result() }
func (r *fakeLiveRecording) Segments() <-chan LiveSegment { return r.segments }
func (r *fakeLiveRecording) Errors() <-chan error         { return r.errors }

func (r *fakeLiveRecording) run(gate <-chan struct{}, readyErr error) {
	defer r.cancel()
	defer close(r.done)
	defer close(r.segments)
	defer close(r.errors)
	if gate != nil {
		select {
		case <-gate:
		case result := <-r.stop:
			err := r.finalize(result)
			r.publishReady(err)
			r.finish(err)
			return
		case <-r.failure:
			err := fakeRecordingFailure(r.failureErr)
			r.publishReady(err)
			r.finish(err)
			return
		case <-r.ctx.Done():
			r.publishReady(r.ctx.Err())
			r.finish(r.ctx.Err())
			return
		}
	}
	if readyErr != nil {
		r.publishReady(readyErr)
		r.finish(readyErr)
		return
	}
	if err := r.appendStep(); err != nil {
		r.publishReady(err)
		r.finish(err)
		return
	}
	r.publishReady(nil)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.failure:
			r.finish(fakeRecordingFailure(r.failureErr))
			return
		case <-r.ctx.Done():
			r.finish(r.ctx.Err())
			return
		case result := <-r.stop:
			r.finish(r.finalize(result))
			return
		case <-ticker.C:
			if err := r.appendStep(); err != nil {
				r.finish(err)
				return
			}
			if err := r.poll(); err != nil {
				r.finish(err)
				return
			}
		}
	}
}

func (r *fakeLiveRecording) appendStep() error {
	r.captureMu.Lock()
	defer r.captureMu.Unlock()
	if r.stopRequested() {
		return nil
	}
	return r.appendStepLocked()
}

func (r *fakeLiveRecording) appendStepLocked() error {
	if r.written >= int64(len(r.data)) {
		return nil
	}
	end := r.written + r.stepBytes
	if end > int64(len(r.data)) {
		end = int64(len(r.data))
	}
	if err := wav.AppendPCM(r.path, r.data[r.written:end]); err != nil {
		return err
	}
	r.written = end
	return nil
}

func (r *fakeLiveRecording) poll() error {
	segments, err := r.segmenter.Poll()
	if err != nil {
		return err
	}
	return r.emit(segments)
}

func (r *fakeLiveRecording) finalize(result fakeStopRequest) error {
	if result.err != nil {
		return result.err
	}
	defer result.file.Close()
	if err := r.poll(); err != nil {
		return err
	}
	segments, err := r.segmenter.Finalize(result.file)
	if err != nil {
		return err
	}
	return r.emit(segments)
}

func (r *fakeLiveRecording) emit(segments []LiveSegment) error {
	for _, segment := range segments {
		select {
		case r.segments <- segment:
		case <-r.ctx.Done():
			return r.ctx.Err()
		}
	}
	return nil
}

func (r *fakeLiveRecording) publishReady(err error) {
	if err != nil {
		r.ready <- err
	}
	close(r.ready)
}

func (r *fakeLiveRecording) finish(err error) {
	r.mu.Lock()
	r.runErr = err
	r.mu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		r.errors <- err
	}
}

func (r *fakeLiveRecording) Stop(ctx context.Context) error {
	select {
	case <-r.done:
		return r.result()
	default:
	}
	owner := false
	r.stopOnce.Do(func() {
		owner = true
		close(r.stopping)
		go func() {
			file, err := r.stopCapture(ctx)
			result := fakeStopRequest{file: file, err: err}
			select {
			case r.stop <- result:
			case <-r.done:
				_ = file.Close()
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

func (r *fakeLiveRecording) stopCapture(ctx context.Context) (wav.File, error) {
	if err := ctx.Err(); err != nil {
		return wav.File{}, err
	}
	r.captureMu.Lock()
	defer r.captureMu.Unlock()
	if r.written < int64(len(r.data)) {
		if err := wav.AppendPCM(r.path, r.data[r.written:]); err != nil {
			return wav.File{}, err
		}
		r.written = int64(len(r.data))
	}
	final, err := wav.FinalizeGrowing(r.path)
	if err != nil {
		return wav.File{}, err
	}
	if final.Duration() < r.opts.MinimumDuration {
		return wav.File{}, errors.Join(errs.New(errs.NoSpeech, "audio/fake", "capture audio vide"), final.Close())
	}
	return final, nil
}

func (r *fakeLiveRecording) stopRequested() bool {
	select {
	case <-r.stopping:
		return true
	default:
		return false
	}
}

func (r *fakeLiveRecording) Abort() error {
	r.cancel()
	<-r.done
	r.cleanup.Do(func() { r.cleanErr = errors.Join(r.segmenter.Close(), os.RemoveAll(r.tempDir)) })
	if err := r.result(); err != nil && !errors.Is(err, context.Canceled) {
		return errors.Join(err, r.cleanErr)
	}
	return r.cleanErr
}

func (r *fakeLiveRecording) Release() (string, error) {
	r.cleanup.Do(func() {
		segmentErr := r.segmenter.Close()
		r.release, r.cleanErr = preserveOrRemove(r.path, r.tempDir, r.opts.Retention)
		r.cleanErr = errors.Join(segmentErr, r.cleanErr)
	})
	return r.release, r.cleanErr
}

func (r *fakeLiveRecording) result() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

var _ LiveRecorder = (*FakeRecorder)(nil)
var _ LiveRecording = (*fakeLiveRecording)(nil)

func liveDurationSamples(duration time.Duration) int64 {
	return int64(duration) * wav.SampleRate / int64(time.Second)
}
