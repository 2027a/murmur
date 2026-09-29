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
	PauseWindow     time.Duration
	Overlap         time.Duration
	MaxSegments     int
}

// ErrSegmentLimit is fatal even when partial STT recovery is enabled.
var ErrSegmentLimit = errs.New(errs.ServerError, "audio", "nombre de segments audio supérieur à la limite configurée")

type LiveSegment struct {
	Index     int
	File      wav.File
	CreatedAt time.Time
	Overlap   bool
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
	Discard() error
}

func ValidateLiveOptions(options LiveOptions) error {
	if options.SegmentDuration <= 0 {
		return errs.New(errs.ConfigInvalid, "audio", "la durée des segments live doit être strictement positive")
	}
	if options.MinimumTail < 0 {
		return errs.New(errs.ConfigInvalid, "audio", "la durée minimale de fin live ne peut pas être négative")
	}
	if options.PauseWindow < 0 || options.PauseWindow >= options.SegmentDuration {
		return errs.New(errs.ConfigInvalid, "audio", "la fenêtre de pause doit être positive ou nulle et inférieure au segment")
	}
	if options.MaxSegments < 0 {
		return errs.New(errs.ConfigInvalid, "audio", "limite de segments négative")
	}
	if options.Overlap < 0 || options.Overlap >= options.SegmentDuration-options.PauseWindow {
		return errs.New(errs.ConfigInvalid, "audio", "le recouvrement doit être positif ou nul et inférieur au pas minimal")
	}
	return nil
}

// GrowingSegmenter only waits for MinimumTail of lookahead, not another full
// segment. The unsubmitted PCM remains in the capture until the next cut/stop.
type GrowingSegmenter struct {
	path           string
	source         *os.File
	outputDir      string
	segmentSamples int64
	minimumTail    int64
	cursor         int64
	pauseWindow    time.Duration
	overlap        int64
	maxSegments    int
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
		pauseWindow: options.PauseWindow, maxSegments: options.MaxSegments,
		overlap: liveDurationSamples(options.Overlap),
	}, nil
}

func (s *GrowingSegmenter) Poll() ([]LiveSegment, error) {
	snapshot, ok, err := s.snapshot()
	if err != nil || !ok {
		return nil, err
	}
	defer snapshot.Close()
	if snapshot.Samples()-s.cursor < s.segmentSamples+s.minimumTail {
		return nil, nil
	}
	// Materialize at most one segment per poll: a slow server must not cause a
	// burst of snapshots proportional to the accumulated recording duration.
	end, err := snapshot.PauseCut(s.cursor+s.segmentSamples, s.pauseWindow)
	if err != nil {
		return nil, err
	}
	part, err := s.cut(snapshot, end)
	if err != nil {
		return nil, err
	}
	return []LiveSegment{part}, nil
}

func (s *GrowingSegmenter) Finalize(final wav.File) ([]LiveSegment, error) {
	if final.Path() != s.path || final.Samples() < s.cursor {
		return nil, errs.New(errs.AudioFailed, "audio", "le WAV final ne correspond pas aux snapshots live")
	}
	var emitted []LiveSegment
	for s.cursor < final.Samples() {
		part, err := s.NextFinal(final)
		if err != nil {
			return nil, err
		}
		emitted = append(emitted, *part)
	}
	return emitted, nil
}

// NextFinal drains one immutable segment at a time under consumer backpressure.
func (s *GrowingSegmenter) NextFinal(final wav.File) (*LiveSegment, error) {
	if final.Path() != s.path || final.Samples() < s.cursor {
		return nil, errs.New(errs.AudioFailed, "audio", "le WAV final ne correspond pas aux snapshots live")
	}
	if final.Samples() == s.cursor {
		return nil, nil
	}
	end := final.Samples()
	if end-s.cursor > s.segmentSamples && end-s.cursor >= s.segmentSamples+s.minimumTail {
		var err error
		end, err = final.PauseCut(s.cursor+s.segmentSamples, s.pauseWindow)
		if err != nil {
			return nil, err
		}
	}
	part, err := s.cut(final, end)
	return &part, err
}

func (s *GrowingSegmenter) cut(source wav.File, end int64) (LiveSegment, error) {
	if s.maxSegments > 0 && s.nextIndex >= s.maxSegments {
		return LiveSegment{}, ErrSegmentLimit
	}
	start := max(int64(0), s.cursor-s.overlap)
	overlap := start < s.cursor
	file, err := source.Slice(s.outputDir, start, end-start)
	if err != nil {
		return LiveSegment{}, err
	}
	s.files = append(s.files, file)
	s.cursor = end
	part := s.segment(file)
	part.Overlap = overlap
	return part, nil
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
	segment := LiveSegment{Index: s.nextIndex, File: file, CreatedAt: time.Now()}
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
	for {
		segment, err := r.segmenter.NextFinal(result.file)
		if err != nil || segment == nil {
			return err
		}
		if err := r.emit([]LiveSegment{*segment}); err != nil {
			return err
		}
	}
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

func (r *fakeLiveRecording) Discard() error {
	<-r.done
	r.cleanup.Do(func() {
		r.cleanErr = errors.Join(r.segmenter.Close(), os.RemoveAll(r.tempDir))
	})
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
