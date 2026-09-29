package events

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTimingClosedSchema(t *testing.T) {
	event := Event{Version: 1, Type: TimingRecorded, Timestamp: time.Now(), SessionID: "test", State: "recording",
		Provider: "local", Partial: 1, Timing: &Timing{Stage: "stt", DurationMS: 123, QueueMS: 12, AudioMS: 5000}}
	data, err := MarshalJSONL(event)
	if err != nil || !strings.Contains(string(data), `"queue_ms":12`) {
		t.Fatalf("%s %v", data, err)
	}
	for _, stage := range []string{"transcript canary", "https://provider", ""} {
		event.Timing.Stage = stage
		if _, err := MarshalJSONL(event); err == nil {
			t.Fatal("arbitrary stage accepted")
		}
	}
	event.Timing.Stage = "stt"
	event.Timing.QueueMS = -1
	if event.Validate() == nil {
		t.Fatal("negative timing accepted")
	}
}

func TestMeasureReportsThroughContext(t *testing.T) {
	var got Timing
	ctx := WithTimings(context.Background(), func(timing Timing, provider string, partial int) {
		got = timing
		if provider != "" || partial != 0 {
			t.Fatal("unexpected provenance")
		}
	})
	Measure(ctx, "cleanup")()
	if got.Stage != "cleanup" || got.DurationMS < 0 {
		t.Fatalf("timing=%#v", got)
	}
}
