package stt

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

type fakeProvider struct {
	name      string
	mu        sync.Mutex
	calls     []string
	audio     []string
	responses map[string]Result
	errors    map[string]error
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Transcribe(_ context.Context, request Request) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, request.Model)
	p.audio = append(p.audio, request.Audio.Path())
	if err := p.errors[request.Model]; err != nil {
		return Result{}, err
	}
	result := p.responses[request.Model]
	result.Provider = p.name
	return result, nil
}

func TestChainFallbackReusesAudio(t *testing.T) {
	audio := writeAudio(t, "audio")
	primary := &fakeProvider{name: "primary", errors: map[string]error{"same": errs.New(errs.Unavailable, "fake", "down")}}
	fallback := &fakeProvider{name: "fallback", responses: map[string]Result{"same": {Text: "ok"}}}
	chain, err := NewChain(primary, fallback, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := chain.Transcribe(context.Background(), Request{Audio: audio, Model: "same"})
	if err != nil || result.Provider != "fallback" || result.Text != "ok" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Class != errs.Unavailable || result.Warnings[0].Provider != "primary" {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	if !reflect.DeepEqual(primary.calls, []string{"same"}) || !reflect.DeepEqual(fallback.calls, []string{"same"}) ||
		!reflect.DeepEqual(primary.audio, []string{audio.Path()}) || !reflect.DeepEqual(fallback.audio, []string{audio.Path()}) {
		t.Fatalf("calls: primary=%v fallback=%v", primary.calls, fallback.calls)
	}
}

func TestSegmentsRestartEntirelyOnFallbackWithoutMixing(t *testing.T) {
	primary := &fakeProvider{
		name: "primary", responses: map[string]Result{"1": {Text: "one"}},
		errors: map[string]error{"2": errs.New(errs.ServerError, "fake", "down")},
	}
	fallback := &fakeProvider{name: "fallback", responses: map[string]Result{
		"1": {Text: "un"}, "2": {Text: "deux"}, "3": {Text: "trois"},
	}}
	chain, _ := NewChain(primary, fallback, MultiOptions{})
	result, err := chain.TranscribeSegments(context.Background(), []Segment{
		{Index: 3, Request: Request{Model: "3"}},
		{Index: 1, Request: Request{Model: "1"}},
		{Index: 2, Request: Request{Model: "2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	warningsOK := len(result.Warnings) == 1 && result.Warnings[0].Class == errs.ServerError &&
		result.Warnings[0].Provider == "primary" && strings.Contains(result.Warnings[0].Message, "fallback STT")
	if result.Text != "un deux trois" || result.Provider != "fallback" || result.Partial || !warningsOK {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(primary.calls, []string{"1", "2"}) || !reflect.DeepEqual(fallback.calls, []string{"1", "2", "3"}) {
		t.Fatalf("calls: primary=%v fallback=%v", primary.calls, fallback.calls)
	}
}

func TestSegmentsRejectFailureByDefault(t *testing.T) {
	provider := &fakeProvider{
		name: "primary", responses: map[string]Result{"1": {Text: "one"}},
		errors: map[string]error{"2": errs.New(errs.ServerError, "fake", "down")},
	}
	chain, _ := NewChain(provider, nil, MultiOptions{})
	result, err := chain.TranscribeSegments(context.Background(), segments("1", "2", "3"))
	if err == nil || result.Text != "" || result.Segments != nil {
		t.Fatalf("partial result leaked: %#v, error=%v", result, err)
	}
}

func TestSegmentsPartialRecoveryIsExplicit(t *testing.T) {
	provider := &fakeProvider{
		name: "primary", responses: map[string]Result{"1": {Text: "one"}, "3": {Text: "three"}},
		errors: map[string]error{"2": errs.New(errs.ServerError, "fake", "down")},
	}
	chain, _ := NewChain(provider, nil, MultiOptions{AllowPartial: true})
	result, err := chain.TranscribeSegments(context.Background(), segments("1", "2", "3"))
	if err != nil {
		t.Fatal(err)
	}
	warningsOK := len(result.Warnings) == 1 && result.Warnings[0].Class == errs.ServerError &&
		result.Warnings[0].Provider == "primary" && result.Warnings[0].Message == WarningPartial
	if result.Text != "one three" || !result.Partial || !warningsOK {
		t.Fatalf("result = %#v", result)
	}
}

func TestSegmentsMixedProvidersRequireOptInAndAreTraced(t *testing.T) {
	primary := &fakeProvider{
		name: "primary", responses: map[string]Result{"1": {Text: "one"}},
		errors: map[string]error{"2": errs.New(errs.ServerError, "fake", "down")},
	}
	fallback := &fakeProvider{name: "fallback", responses: map[string]Result{"2": {Text: "two"}}}
	chain, _ := NewChain(primary, fallback, MultiOptions{AllowMixedProviders: true})
	result, err := chain.TranscribeSegments(context.Background(), segments("1", "2"))
	if err != nil {
		t.Fatal(err)
	}
	warningsOK := len(result.Warnings) == 2 && result.Warnings[0].Class == errs.ServerError &&
		result.Warnings[0].Provider == "primary" && strings.Contains(result.Warnings[0].Message, "fallback STT") &&
		result.Warnings[1].Class == errs.Unavailable && result.Warnings[1].Message == WarningMixedProviders
	if result.Provider != "" || result.Text != "one two" || !warningsOK {
		t.Fatalf("result = %#v", result)
	}
	want := []SegmentResult{{Index: 0, Text: "one", Provider: "primary"}, {Index: 1, Text: "two", Provider: "fallback"}}
	if !reflect.DeepEqual(result.Segments, want) {
		t.Fatalf("segments = %#v, want %#v", result.Segments, want)
	}
}

func TestDoubleFailurePreservesCausesWithoutLeakingMessages(t *testing.T) {
	primaryErr := errors.New("primary-secret")
	fallbackErr := errors.New("fallback-secret")
	primary := &fakeProvider{name: "primary", errors: map[string]error{"1": primaryErr}}
	fallback := &fakeProvider{name: "fallback", errors: map[string]error{"1": fallbackErr}}
	chain, _ := NewChain(primary, fallback, MultiOptions{})

	_, err := chain.TranscribeSegments(context.Background(), segments("1"))
	if !errors.Is(err, primaryErr) || !errors.Is(err, fallbackErr) {
		t.Fatalf("causes perdues: %v", err)
	}
	if strings.Contains(err.Error(), "primary-secret") || strings.Contains(err.Error(), "fallback-secret") {
		t.Fatalf("message sensible exposé: %v", err)
	}
	if got := ProviderFromError(err); got != "fallback" {
		t.Fatalf("provider = %q", got)
	}
}

func TestStreamWorkerPoolIsBoundedAndResultsAreOrdered(t *testing.T) {
	var active, maximum atomic.Int32
	provider := providerFunc{name: "primary", transcribe: func(_ context.Context, request Request) (Result, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		index, _ := strconv.Atoi(request.Model)
		time.Sleep(time.Duration(4-index) * 10 * time.Millisecond)
		return Result{Text: request.Model}, nil
	}}
	chain, err := NewChain(provider, nil, MultiOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan Segment, 4)
	for index := range 4 {
		input <- Segment{Index: index, Request: Request{Model: strconv.Itoa(index)}}
	}
	close(input)
	result, err := chain.TranscribeStream(context.Background(), input, Progress{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "0 1 2 3" || maximum.Load() != 2 {
		t.Fatalf("result=%#v maximum=%d", result, maximum.Load())
	}
}

func TestStreamFailurePoliciesRemainExplicit(t *testing.T) {
	primary := &fakeProvider{
		name: "primary", responses: map[string]Result{"0": {Text: "zero"}, "2": {Text: "two"}},
		errors: map[string]error{"1": errs.New(errs.ServerError, "fake", "down")},
	}
	for _, test := range []struct {
		name    string
		options MultiOptions
		wantErr bool
		want    string
	}{
		{name: "default rejects", options: MultiOptions{Workers: 2}, wantErr: true},
		{name: "partial opt-in", options: MultiOptions{Workers: 2, AllowPartial: true}, want: "zero two"},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain, err := NewChain(primary, nil, test.options)
			if err != nil {
				t.Fatal(err)
			}
			input := make(chan Segment, 3)
			for index := range 3 {
				input <- Segment{Index: index, Request: Request{Model: strconv.Itoa(index)}}
			}
			close(input)
			result, err := chain.TranscribeStream(context.Background(), input, Progress{})
			if (err != nil) != test.wantErr || result.Text != test.want {
				t.Fatalf("result=%#v error=%v", result, err)
			}
			if !test.wantErr && (len(result.Warnings) != 1 || result.Warnings[0].Message != WarningPartial) {
				t.Fatalf("warnings=%#v", result.Warnings)
			}
		})
	}
}

func TestStreamCancellationWaitsForEveryWorker(t *testing.T) {
	started := make(chan struct{}, 2)
	var active atomic.Int32
	provider := providerFunc{name: "primary", transcribe: func(ctx context.Context, _ Request) (Result, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- struct{}{}
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	chain, err := NewChain(provider, nil, MultiOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan Segment, 4)
	for index := range 4 {
		input <- Segment{Index: index, Request: Request{Model: strconv.Itoa(index)}}
	}
	close(input)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := chain.TranscribeStream(ctx, input, Progress{})
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers non démarrés")
		}
	}
	cancel()
	select {
	case err := <-done:
		if errs.Classify(err) != errs.Cancelled || active.Load() != 0 {
			t.Fatalf("error=%v workers actifs=%d", err, active.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("pool non drainé après annulation")
	}
}

type providerFunc struct {
	name       string
	transcribe func(context.Context, Request) (Result, error)
}

func (p providerFunc) Name() string { return p.name }
func (p providerFunc) Transcribe(ctx context.Context, request Request) (Result, error) {
	return p.transcribe(ctx, request)
}

func segments(paths ...string) []Segment {
	result := make([]Segment, len(paths))
	for index, path := range paths {
		result[index] = Segment{Index: index, Request: Request{Model: path}}
	}
	return result
}
