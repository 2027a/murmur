// Package pwrecord captures Murmur's canonical WAV format with pw-record.
package pwrecord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

const (
	defaultStopGrace  = 2 * time.Second
	defaultAbortGrace = 200 * time.Millisecond
	defaultKillWait   = 2 * time.Second
)

type Recorder struct {
	executor sysexec.Executor
}

func New(executor sysexec.Executor) *Recorder {
	return &Recorder{executor: executor}
}

func (r *Recorder) Start(ctx context.Context, opts audio.RecordOptions) (audio.Recording, error) {
	if r.executor == nil {
		return nil, errs.New(errs.Internal, "audio/pw-record", "exécuteur système absent")
	}
	normalized, err := audio.NormalizeRecordOptions(opts)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp(normalized.Directory, "murmur-audio-")
	if err != nil {
		return nil, errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible de créer le répertoire de capture", err)
	}
	path := filepath.Join(tempDir, "recording.wav")
	args := []string{
		"--media-category", "Capture",
		"--media-role", "Communication",
		"--rate", "16000",
		"--channels", "1",
		"--format", "s16",
	}
	if normalized.Target != "" {
		args = append(args, "--target", normalized.Target)
	}
	args = append(args, path)
	process, err := r.executor.Start(ctx, "pw-record", args)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible de démarrer pw-record", err)
	}

	recording := &recording{
		process:    process,
		path:       path,
		tempDir:    tempDir,
		opts:       normalized,
		stopGrace:  defaultStopGrace,
		abortGrace: defaultAbortGrace,
		killWait:   defaultKillWait,
		ready:      make(chan error, 1),
		procDone:   make(chan struct{}),
		finished:   make(chan struct{}),
	}
	go recording.waitProcess()
	go recording.watchReadiness(ctx)
	go recording.watchCancellation(ctx)
	if normalized.Meter != nil {
		go recording.watchLevels(ctx)
	}
	return recording, nil
}

type recording struct {
	process    sysexec.Process
	path       string
	tempDir    string
	opts       audio.RecordOptions
	stopGrace  time.Duration
	abortGrace time.Duration
	killWait   time.Duration

	readyOnce sync.Once
	ready     chan error
	procDone  chan struct{}
	procErr   error
	doneErr   error

	mu         sync.Mutex
	action     action
	finished   chan struct{}
	result     audio.WavFile
	resultErr  error
	abortCause error
}

type action uint8

const (
	actionNone action = iota
	actionStopping
	actionAborting
)

func (r *recording) Ready() <-chan error { return r.ready }

func (r *recording) Done() <-chan struct{} { return r.procDone }

func (r *recording) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doneErr
}

func (r *recording) waitProcess() {
	procErr := r.process.Wait()
	r.mu.Lock()
	r.procErr = procErr
	if r.action == actionNone {
		r.doneErr = processFailure(procErr, "pw-record s'est arrêté pendant la capture")
	}
	r.mu.Unlock()
	close(r.procDone)
}

func (r *recording) watchReadiness(ctx context.Context) {
	timer := time.NewTimer(r.opts.ReadinessTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(r.opts.ReadinessInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.publishReady(ctx.Err())
			return
		case <-timer.C:
			err := errs.New(errs.Timeout, "audio/pw-record", "pw-record n'a produit aucune donnée audio à temps")
			r.publishReady(err)
			_ = r.abort(err)
			return
		case <-r.procDone:
			if ctx.Err() != nil {
				r.publishReady(ctx.Err())
			} else {
				err := r.processFailure("pw-record s'est arrêté avant la première donnée audio")
				r.publishReady(err)
				_ = r.abort(err)
			}
			return
		case <-ticker.C:
			ready, err := wav.HasAudioData(r.path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				r.publishReady(err)
				_ = r.abort(err)
				return
			}
			if ready {
				r.publishReady(nil)
				return
			}
		}
	}
}

func (r *recording) watchCancellation(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = r.abort(ctx.Err())
	case <-r.finished:
	}
}

func (r *recording) publishReady(err error) {
	r.readyOnce.Do(func() {
		if err != nil {
			r.ready <- err
		}
		close(r.ready)
	})
}

func (r *recording) Stop(ctx context.Context) (audio.WavFile, error) {
	owner, _, endedErr := r.begin(actionStopping, nil)
	if !owner {
		return r.observeFinished(ctx, r.stopGrace+r.killWait)
	}
	if endedErr != nil {
		r.complete(audio.WavFile{}, endedErr, true)
		return r.result, r.resultErr
	}

	graceful, err := r.terminate(ctx, r.stopGrace)
	if err != nil {
		r.complete(audio.WavFile{}, err, false)
		return r.result, r.resultErr
	}
	if !graceful {
		r.complete(audio.WavFile{}, errs.New(errs.Timeout, "audio/pw-record", "pw-record n'a pas finalisé la capture avant son arrêt forcé"), false)
		return r.result, r.resultErr
	}
	if cause := r.cancellationCause(); cause != nil {
		r.complete(audio.WavFile{}, cause, false)
		return r.result, r.resultErr
	}
	if ctx.Err() != nil {
		r.complete(audio.WavFile{}, ctx.Err(), false)
		return r.result, r.resultErr
	}
	if err := os.Chmod(r.path, 0o600); err != nil {
		r.complete(audio.WavFile{}, errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible de protéger le fichier audio", err), true)
		return r.result, r.resultErr
	}
	file, err := wav.Open(r.path)
	if err != nil {
		r.complete(audio.WavFile{}, err, true)
		return r.result, r.resultErr
	}
	if file.Duration() < r.opts.MinimumDuration {
		r.complete(audio.WavFile{}, errors.Join(errs.New(errs.NoSpeech, "audio/pw-record", "capture audio vide"), file.Close()), true)
		return r.result, r.resultErr
	}
	r.complete(audio.NewWavFile(file, r.tempDir, r.opts.Retention), nil, false)
	return r.result, nil
}

func (r *recording) Abort() error { return r.abort(nil) }

func (r *recording) abort(cause error) error {
	owner, _, endedErr := r.begin(actionAborting, cause)
	if !owner {
		_, err := r.observeFinished(context.Background(), r.stopGrace+r.killWait)
		return err
	}
	if endedErr != nil {
		cleanupErr := os.RemoveAll(r.tempDir)
		if cleanupErr != nil {
			cleanupErr = errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible de supprimer la capture annulée", cleanupErr)
		}
		r.complete(audio.WavFile{}, errors.Join(endedErr, cleanupErr), false)
		return r.resultErr
	}
	_, stopErr := r.terminate(context.Background(), r.abortGrace)
	cleanupErr := os.RemoveAll(r.tempDir)
	if cleanupErr != nil {
		cleanupErr = errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible de supprimer la capture annulée", cleanupErr)
	}
	r.complete(audio.WavFile{}, errors.Join(cause, stopErr, cleanupErr), false)
	return r.resultErr
}

func (r *recording) terminate(ctx context.Context, grace time.Duration) (bool, error) {
	signalErr := r.process.Signal(os.Interrupt)
	if errors.Is(signalErr, os.ErrProcessDone) {
		signalErr = nil
	}
	if signalErr == nil {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-r.procDone:
			return true, nil
		case <-ctx.Done():
			signalErr = ctx.Err()
		case <-timer.C:
		}
	}

	killErr := r.process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	timer := time.NewTimer(r.killWait)
	defer timer.Stop()
	select {
	case <-r.procDone:
	case <-timer.C:
		return false, errors.Join(signalErr, killErr, errs.New(errs.Timeout, "audio/pw-record", "arrêt forcé de pw-record non confirmé"))
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if signalErr != nil {
		return false, errors.Join(errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible d'interrompre proprement pw-record", signalErr), killErr)
	}
	if killErr != nil {
		return false, errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible d'arrêter pw-record", killErr)
	}
	return false, nil
}

func (r *recording) begin(next action, cause error) (bool, action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.action != actionNone {
		return false, r.action, nil
	}
	endedErr := r.doneErr
	r.action = next
	r.abortCause = cause
	return true, next, endedErr
}

func (r *recording) cancellationCause() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortCause
}

func (r *recording) complete(file audio.WavFile, err error, retainFailure bool) {
	if retainFailure {
		_, cleanupErr := audio.RetainCapture(r.path, r.tempDir, r.opts.Retention)
		err = errors.Join(err, cleanupErr)
	} else if err != nil {
		cleanupErr := os.RemoveAll(r.tempDir)
		if cleanupErr != nil {
			cleanupErr = errs.Wrap(errs.AudioFailed, "audio/pw-record", "impossible de supprimer la capture", cleanupErr)
		}
		err = errors.Join(err, cleanupErr)
	}
	r.mu.Lock()
	r.result = file
	r.resultErr = err
	close(r.finished)
	r.mu.Unlock()
}

func (r *recording) observeFinished(ctx context.Context, timeout time.Duration) (audio.WavFile, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-r.finished:
		return r.result, r.resultErr
	case <-ctx.Done():
		return audio.WavFile{}, ctx.Err()
	case <-timer.C:
		return audio.WavFile{}, errs.New(errs.Timeout, "audio/pw-record", "arrêt de pw-record non confirmé")
	}
}

func (r *recording) processFailure(message string) error {
	return processFailure(r.procErr, message)
}

func processFailure(err error, message string) error {
	if err == nil {
		return errs.New(errs.AudioFailed, "audio/pw-record", message)
	}
	return errs.Wrap(errs.AudioFailed, "audio/pw-record", message, err)
}

var _ audio.Recorder = (*Recorder)(nil)
var _ audio.Recording = (*recording)(nil)
