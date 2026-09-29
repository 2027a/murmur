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

type Recorder interface {
	Start(ctx context.Context, opts RecordOptions) (Recording, error)
}

type Recording interface {
	Ready() <-chan error
	Done() <-chan struct{}
	Err() error
	Stop(ctx context.Context) (WavFile, error)
	Abort() error
}

type RecordOptions struct {
	Directory         string
	Target            string
	ReadinessTimeout  time.Duration
	ReadinessInterval time.Duration
	MinimumDuration   time.Duration
	Retention         RetentionOptions
	// Meter must return immediately; it runs independently of capture/STT.
	Meter func(wav.Level)
}

type WavFile struct {
	wav.File
	cleanup *cleanupState
}

// Release removes the temporary recording or moves it into the bounded
// diagnostic store when retention was explicitly enabled.
func (f WavFile) Release() (string, error) {
	if f.cleanup == nil {
		return "", f.File.Close()
	}
	f.cleanup.once.Do(func() {
		if f.cleanup.retention.Keep {
			f.cleanup.diagnostic, f.cleanup.err = preserveValidatedOrRemove(f.File, f.cleanup.tempDir, f.cleanup.retention)
			return
		}
		closeErr := f.File.Close()
		f.cleanup.diagnostic, f.cleanup.err = preserveOrRemove(f.Path(), f.cleanup.tempDir, f.cleanup.retention)
		f.cleanup.err = errors.Join(closeErr, f.cleanup.err)
	})
	return f.cleanup.diagnostic, f.cleanup.err
}

// Discard closes the recording and removes its temporary capture, regardless
// of the configured diagnostic retention. Only the first cleanup decision wins.
func (f WavFile) Discard() error {
	if f.cleanup == nil {
		return f.File.Close()
	}
	f.cleanup.once.Do(func() {
		f.cleanup.err = errors.Join(f.File.Close(), removeCapture(f.cleanup.tempDir))
	})
	return f.cleanup.err
}

type cleanupState struct {
	once       sync.Once
	tempDir    string
	retention  RetentionOptions
	diagnostic string
	err        error
}

type FakeRecorder struct {
	FixturePath      string
	StartErr         error
	ReadyErr         error
	ReadyGate        <-chan struct{}
	DoneGate         <-chan struct{}
	DoneErr          error
	LiveStepSamples  int64
	LiveStepInterval time.Duration
}

func (f *FakeRecorder) Start(ctx context.Context, opts RecordOptions) (Recording, error) {
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
	tempDir, err := os.MkdirTemp(opts.Directory, "murmur-audio-")
	if err != nil {
		return nil, errs.Wrap(errs.AudioFailed, "audio/fake", "impossible de créer le répertoire de capture", err)
	}
	path := filepath.Join(tempDir, "recording.wav")
	if err := copyFile(f.FixturePath, path); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, errs.Wrap(errs.AudioFailed, "audio/fake", "impossible de copier la fixture audio", err)
	}
	recording := &fakeRecording{
		path:    path,
		tempDir: tempDir,
		opts:    opts,
		ready:   make(chan error, 1),
		done:    make(chan struct{}),
	}
	go recording.run(ctx, f.ReadyGate, f.ReadyErr, f.DoneGate, f.DoneErr)
	return recording, nil
}

type fakeRecording struct {
	mu        sync.Mutex
	path      string
	tempDir   string
	opts      RecordOptions
	readyOnce sync.Once
	ready     chan error
	done      chan struct{}
	finished  bool
	doneErr   error
	cleanup   sync.Once
	cleanErr  error
}

func (r *fakeRecording) Ready() <-chan error { return r.ready }

func (r *fakeRecording) Done() <-chan struct{} { return r.done }

func (r *fakeRecording) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doneErr
}

func (r *fakeRecording) run(ctx context.Context, readyGate <-chan struct{}, readyErr error, doneGate <-chan struct{}, doneErr error) {
	if readyGate != nil {
		select {
		case <-readyGate:
		case <-doneGate:
			err := fakeRecordingFailure(doneErr)
			r.publishReadyResult(err)
			r.finish(err)
			return
		case <-ctx.Done():
			r.publishReadyResult(ctx.Err())
			r.finish(ctx.Err())
			return
		case <-r.done:
			r.publishReadyResult(context.Canceled)
			return
		}
	}
	r.publishReadyResult(readyErr)
	if readyErr != nil {
		r.finish(readyErr)
		return
	}
	select {
	case <-doneGate:
		r.finish(fakeRecordingFailure(doneErr))
	case <-ctx.Done():
		_ = r.Abort()
	case <-r.done:
	}
}

func (r *fakeRecording) publishReadyResult(err error) {
	r.readyOnce.Do(func() {
		if err != nil {
			r.ready <- err
		}
		close(r.ready)
	})
}

func (r *fakeRecording) Stop(ctx context.Context) (WavFile, error) {
	if err := ctx.Err(); err != nil {
		_ = r.Abort()
		return WavFile{}, err
	}
	r.mu.Lock()
	if r.finished {
		err := r.doneErr
		r.mu.Unlock()
		if err != nil {
			return WavFile{}, err
		}
		return WavFile{}, errs.New(errs.AudioFailed, "audio/fake", "capture déjà terminée")
	}
	r.finished = true
	close(r.done)
	r.mu.Unlock()

	file, err := wav.Open(r.path)
	if err != nil {
		_, cleanupErr := preserveOrRemove(r.path, r.tempDir, r.opts.Retention)
		return WavFile{}, errors.Join(err, cleanupErr)
	}
	if file.Duration() < r.opts.MinimumDuration {
		closeErr := file.Close()
		_, cleanupErr := preserveOrRemove(r.path, r.tempDir, r.opts.Retention)
		return WavFile{}, errors.Join(errs.New(errs.NoSpeech, "audio/fake", "capture audio vide"), closeErr, cleanupErr)
	}
	return newWavFile(file, r.tempDir, r.opts.Retention), nil
}

func (r *fakeRecording) Abort() error {
	r.finish(nil)
	r.cleanup.Do(func() { r.cleanErr = removeCapture(r.tempDir) })
	return r.cleanErr
}

func (r *fakeRecording) finish(err error) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.doneErr = err
	close(r.done)
	r.mu.Unlock()
}

func fakeRecordingFailure(err error) error {
	if err == nil {
		return errs.New(errs.AudioFailed, "audio/fake", "capture audio interrompue")
	}
	return errs.Wrap(errs.AudioFailed, "audio/fake", "capture audio interrompue", err)
}

func newWavFile(file wav.File, tempDir string, retention RetentionOptions) WavFile {
	return WavFile{File: file, cleanup: &cleanupState{tempDir: tempDir, retention: retention}}
}

func NewWavFile(file wav.File, tempDir string, retention RetentionOptions) WavFile {
	return newWavFile(file, tempDir, retention)
}

func normalizeOptions(opts RecordOptions) (RecordOptions, error) {
	if opts.ReadinessTimeout < 0 || opts.ReadinessInterval < 0 || opts.MinimumDuration < 0 {
		return RecordOptions{}, errs.New(errs.ConfigInvalid, "audio", "les durées de capture ne peuvent pas être négatives")
	}
	if opts.ReadinessTimeout == 0 {
		opts.ReadinessTimeout = 5 * time.Second
	}
	if opts.ReadinessInterval == 0 {
		opts.ReadinessInterval = 20 * time.Millisecond
	}
	if opts.MinimumDuration == 0 {
		opts.MinimumDuration = 100 * time.Millisecond
	}
	if err := opts.Retention.validate(); err != nil {
		return RecordOptions{}, err
	}
	return opts, nil
}

func NormalizeRecordOptions(opts RecordOptions) (RecordOptions, error) {
	return normalizeOptions(opts)
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func removeCapture(tempDir string) error {
	if tempDir == "" {
		return nil
	}
	return os.RemoveAll(tempDir)
}

var _ Recorder = (*FakeRecorder)(nil)
var _ Recording = (*fakeRecording)(nil)
