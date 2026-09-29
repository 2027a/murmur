// latency-bench replays the same canonical WAV through Murmur's real providers
// and processing pipeline. It never captures a microphone or injects text.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/config"
	"git.2027a.net/2027a/murmur/internal/core"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/ipc"
	"git.2027a.net/2027a/murmur/internal/session"
	"git.2027a.net/2027a/murmur/internal/stt"
)

type report struct {
	Mode         string             `json:"mode"`
	Run          int                `json:"run"`
	ChunkSeconds int                `json:"chunk_seconds"`
	PauseMS      int                `json:"pause_ms"`
	OverlapMS    int                `json:"overlap_ms"`
	Workers      int                `json:"workers"`
	Model        string             `json:"model"`
	Language     string             `json:"language"`
	Cleanup      bool               `json:"cleanup"`
	AudioMS      int64              `json:"audio_ms"`
	ElapsedMS    int64              `json:"elapsed_ms"`
	PeakRSSKiB   int64              `json:"process_peak_rss_kib"`
	UserCPUMS    int64              `json:"user_cpu_ms"`
	SystemCPUMS  int64              `json:"system_cpu_ms"`
	FirstSTTMS   *int64             `json:"first_stt_ms"`
	Timings      map[string][]int64 `json:"timings_ms"`
	Requests     int                `json:"requests"`
	MaxActive    int                `json:"max_active"`
	Result       string             `json:"result"`
	Error        errs.Class         `json:"error,omitempty"`
	Go           string             `json:"go"`
	Platform     string             `json:"platform"`
	start        time.Time
	active       int
}

func (r *report) observe(event events.Event) {
	switch event.Type {
	case events.PartialStarted:
		if r.FirstSTTMS == nil {
			ms := event.Timestamp.Sub(r.start).Milliseconds()
			r.FirstSTTMS = &ms
		}
		r.Requests++
		r.active++
		r.MaxActive = max(r.MaxActive, r.active)
	case events.PartialDone, events.PartialFailed:
		r.active--
	case events.TimingRecorded:
		if event.Timing != nil {
			t := event.Timing
			r.Timings[t.Stage] = append(r.Timings[t.Stage], t.DurationMS)
			if t.Stage == "stt" {
				r.Timings["queue"] = append(r.Timings["queue"], t.QueueMS)
			}
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "benchmark:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("wav", "", "WAV PCM s16 mono 16 kHz")
	configPath := flag.String("config", "", "configuration Murmur (défaut XDG)")
	mode := flag.String("mode", "live", "live, batch ou file")
	chunk := flag.Int("chunk", 5, "secondes par segment; 0 désactive le découpage")
	pause := flag.Int("pause-ms", 500, "fenêtre de recherche de pause; 0 = coupe fixe")
	overlap := flag.Int("overlap-ms", 0, "recouvrement expérimental, 0 à 1000 ms")
	workers := flag.Int("workers", 2, "concurrence STT")
	repeats := flag.Int("repeat", 5, "répétitions")
	textOut := flag.String("text-out", "", "préfixe de fichiers texte pour correction manuelle (opt-in)")
	reportOut := flag.String("report-out", "", "fichier JSONL de mesures à créer (sans transcript)")
	flag.Parse()
	if *path == "" || *repeats < 1 || (*mode != "live" && *mode != "batch" && *mode != "file") {
		return fmt.Errorf("utiliser -wav fichier.wav -mode live|batch|file -repeat N")
	}
	var reports io.Writer = os.Stdout
	if *reportOut != "" {
		file, err := os.OpenFile(*reportOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		reports = io.MultiWriter(os.Stdout, file)
	}
	cfg, err := config.Resolve(*configPath, config.Overrides{})
	if err != nil {
		return err
	}
	cfg.STT.ChunkSeconds, cfg.STT.ChunkPauseMS, cfg.STT.ChunkWorkers = *chunk, *pause, *workers
	cfg.STT.ChunkOverlapMS = *overlap
	cfg.STT.LiveChunking = *mode == "live"
	cfg.Sink.Mode, cfg.Sink.ClipboardMode, cfg.UI.Mode, cfg.UI.Sound = "stdout", "none", "none", false
	cfg.Audio.KeepRecordings, cfg.Policy.RetainDiagnostics = false, false
	if err := config.Validate(cfg); err != nil {
		return err
	}
	file, err := wav.Open(*path)
	if err != nil {
		return err
	}
	duration := file.Duration()
	if err := file.Close(); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	for run := 1; run <= *repeats; run++ {
		var before, after syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
			return err
		}
		provider := cfg.STT.Providers[cfg.STT.Provider]
		r := report{Mode: *mode, Run: run, ChunkSeconds: *chunk, PauseMS: *pause, Workers: *workers,
			OverlapMS: *overlap,
			Model:     provider.Model, Language: provider.Language, Cleanup: cfg.Cleanup.Enabled,
			AudioMS: duration.Milliseconds(), Timings: make(map[string][]int64), Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
		r.start = time.Now()
		var text string
		if *mode == "file" {
			text, err = runFile(ctx, cfg, *path, &r)
		} else {
			text, err = runSession(ctx, cfg, *path, duration, &r)
		}
		r.ElapsedMS = time.Since(r.start).Milliseconds()
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
			return err
		}
		r.PeakRSSKiB = peakRSS()
		r.UserCPUMS = cpuMS(after.Utime) - cpuMS(before.Utime)
		r.SystemCPUMS = cpuMS(after.Stime) - cpuMS(before.Stime)
		if err != nil {
			r.Error = errs.Classify(err)
			r.Result = "failed"
		}
		if err := json.NewEncoder(reports).Encode(r); err != nil {
			return err
		}
		if *textOut != "" && err == nil {
			output, writeErr := os.OpenFile(fmt.Sprintf("%s-%s-%d.txt", *textOut, *mode, run), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if writeErr != nil {
				return writeErr
			}
			_, writeErr = output.WriteString(text)
			closeErr := output.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if err != nil {
			return fmt.Errorf("échec du pipeline (%s)", errs.Classify(err))
		}
	}
	return nil
}

func cpuMS(value syscall.Timeval) int64 { return value.Sec*1000 + value.Usec/1000 }

// getrusage's high-water mark can include the harness before exec. VmHWM is
// scoped to this executable's current address space on Linux.
func peakRSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmHWM:" {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value
		}
	}
	return 0
}

func runSession(ctx context.Context, cfg config.Config, path string, duration time.Duration, r *report) (string, error) {
	dir, err := os.MkdirTemp("", "murmur-latency-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	bus := events.NewBus()
	stream, unsubscribe := bus.Subscribe(4096)
	defer unsubscribe()
	ready, observed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(observed)
		for event := range stream {
			r.observe(event)
			if event.Type == events.CaptureReady {
				close(ready)
			}
		}
	}()
	defer func() { bus.Close(); <-observed }()
	var output bytes.Buffer
	o, err := session.Build(session.BuildOptions{
		Context: ctx, Config: cfg, RuntimeDir: dir, SessionID: "benchmark", Stdout: &output, Bus: bus,
		Recorder: &audio.FakeRecorder{FixturePath: path, LiveStepSamples: wav.SampleRate / 50, LiveStepInterval: 20 * time.Millisecond},
	})
	if err != nil {
		return "", err
	}
	type outcome struct {
		result session.LastResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr, persistErr := o.Run()
		if runErr == nil {
			runErr = persistErr
		}
		done <- outcome{result, runErr}
	}()
	var result outcome
	select {
	case result = <-done:
	case <-ready:
		timer := time.NewTimer(duration)
		select {
		case result = <-done:
		case <-ctx.Done():
			result = <-done
		case <-timer.C:
			o.Handle(ctx, ipc.Request{Command: ipc.Stop})
			result = <-done
		}
		timer.Stop()
	}
	r.Result = string(result.result.Result)
	return output.String(), result.err
}

func runFile(ctx context.Context, cfg config.Config, path string, r *report) (string, error) {
	// Serialize callbacks through the same bounded event bus as sessions.
	bus := events.NewBus()
	stream, unsubscribe := bus.Subscribe(4096)
	defer unsubscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range stream {
			r.observe(event)
		}
	}()
	defer func() { bus.Close(); <-done }()
	ctx = events.WithTimings(ctx, func(t events.Timing, provider string, partial int) {
		bus.Publish(events.Event{Type: events.TimingRecorded, Timing: &t, Provider: provider, Partial: partial})
	})
	chain, err := core.BuildTranscriber(ctx, cfg, core.BuildOptions{})
	if err != nil {
		return "", err
	}
	processor, err := core.BuildProcessor(ctx, cfg, core.BuildOptions{})
	if err != nil {
		return "", err
	}
	progress := func(kind events.Type) func(int, string) {
		return func(index int, provider string) {
			bus.Publish(events.Event{Type: kind, Timestamp: time.Now(), Partial: index + 1, Provider: provider})
		}
	}
	sttTiming := events.Measure(ctx, "stt_complete")
	result, err := core.TranscribeFileWithOptions(ctx, path, audio.ChunkOptions{
		Duration: time.Duration(cfg.STT.ChunkSeconds) * time.Second, MinTail: time.Second, PauseWindow: time.Duration(cfg.STT.ChunkPauseMS) * time.Millisecond,
		Overlap: time.Duration(cfg.STT.ChunkOverlapMS) * time.Millisecond,
	}, chain, stt.Progress{Started: progress(events.PartialStarted), Done: progress(events.PartialDone),
		Failed: func(i int, p string, _ errs.Class) { progress(events.PartialFailed)(i, p) }})
	sttTiming()
	if err != nil {
		return "", err
	}
	processed, err := processor.Run(ctx, result.Text)
	if err == nil {
		r.Result = "completed"
	}
	return processed.Text, err
}
