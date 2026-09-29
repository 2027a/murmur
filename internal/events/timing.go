package events

import (
	"context"
	"time"
)

// Timing contains only durations. Stage names are a closed vocabulary; no
// transcript, URL, prompt or provider response can enter this payload.
type Timing struct {
	Stage      string `json:"stage"`
	DurationMS int64  `json:"duration_ms"`
	QueueMS    int64  `json:"queue_ms,omitempty"`
	AudioMS    int64  `json:"audio_ms,omitempty"`
}

type timingKey struct{}
type TimingObserver func(Timing, string, int)

func WithTimings(ctx context.Context, observer TimingObserver) context.Context {
	return context.WithValue(ctx, timingKey{}, observer)
}

func ReportTiming(ctx context.Context, timing Timing, provider string, partial int) {
	if observer, ok := ctx.Value(timingKey{}).(TimingObserver); ok && observer != nil {
		observer(timing, provider, partial)
	}
}

func Measure(ctx context.Context, stage string) func() {
	start := time.Now()
	return func() {
		ReportTiming(ctx, Timing{Stage: stage, DurationMS: time.Since(start).Milliseconds()}, "", 0)
	}
}
