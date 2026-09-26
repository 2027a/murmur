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

	"git.2027a.net/2027a/murmur/internal/errs"
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
	}, Progress{})
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
	result, err := chain.TranscribeSegments(context.Background(), segments("1", "2", "3"), Progress{})
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
	result, err := chain.TranscribeSegments(context.Background(), segments("1", "2", "3"), Progress{})
	if err != nil {
		t.Fatal(err)
	}
	warningsOK := len(result.Warnings) == 1 && result.Warnings[0].Class == errs.ServerError &&
		result.Warnings[0].Provider == "primary" && result.Warnings[0].Message == WarningPartial
	if result.Text != "one three" || !result.Partial || !warningsOK {
		t.Fatalf("result = %#v", result)
	}
}

func TestBatchProgressReportsOnlyRealAttempts(t *testing.T) {
	t.Run("partial intermediate failure", func(t *testing.T) {
		provider := &fakeProvider{
			name: "primary", responses: map[string]Result{"0": {Text: "zero"}, "2": {Text: "two"}},
			errors: map[string]error{"1": errs.New(errs.ServerError, "fake", "down")},
		}
		chain, _ := NewChain(provider, nil, MultiOptions{AllowPartial: true})
		var got []progressRecord
		result, err := chain.TranscribeSegments(context.Background(), segments("0", "1", "2"), recordProgress(&got))
		if err != nil {
			t.Fatal(err)
		}
		want := []progressRecord{
			{kind: "started", index: 0, provider: "primary"},
			{kind: "done", index: 0, provider: "primary"},
			{kind: "started", index: 1, provider: "primary"},
			{kind: "failed", index: 1, provider: "primary", class: errs.ServerError},
			{kind: "started", index: 2, provider: "primary"},
			{kind: "done", index: 2, provider: "primary"},
		}
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(result.Segments, []SegmentResult{
			{Index: 0, Text: "zero", Provider: "primary"},
			{Index: 2, Text: "two", Provider: "primary"},
		}) {
			t.Fatalf("progress=%#v result=%#v", got, result)
		}
	})

	t.Run("early stop", func(t *testing.T) {
		provider := &fakeProvider{
			name: "primary", responses: map[string]Result{"0": {Text: "zero"}, "2": {Text: "two"}},
			errors: map[string]error{"1": errs.New(errs.ServerError, "fake", "down")},
		}
		chain, _ := NewChain(provider, nil, MultiOptions{})
		var got []progressRecord
		_, err := chain.TranscribeSegments(context.Background(), segments("0", "1", "2"), recordProgress(&got))
		if err == nil {
			t.Fatal("expected failure")
		}
		want := []progressRecord{
			{kind: "started", index: 0, provider: "primary"},
			{kind: "done", index: 0, provider: "primary"},
			{kind: "started", index: 1, provider: "primary"},
			{kind: "failed", index: 1, provider: "primary", class: errs.ServerError},
		}
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(provider.calls, []string{"0", "1"}) {
			t.Fatalf("progress=%#v calls=%v", got, provider.calls)
		}
	})

	t.Run("cancel before attempt", func(t *testing.T) {
		provider := &fakeProvider{name: "primary", responses: map[string]Result{"0": {Text: "zero"}}}
		chain, _ := NewChain(provider, nil, MultiOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var got []progressRecord
		_, err := chain.TranscribeSegments(ctx, segments("0"), recordProgress(&got))
		if errs.Classify(err) != errs.Cancelled || len(got) != 0 || len(provider.calls) != 0 {
			t.Fatalf("error=%v progress=%#v calls=%v", err, got, provider.calls)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		primary := &fakeProvider{
			name: "primary", responses: map[string]Result{"0": {Text: "zero"}},
			errors: map[string]error{"1": errs.New(errs.ServerError, "fake", "down")},
		}
		fallback := &fakeProvider{name: "fallback", responses: map[string]Result{
			"0": {Text: "zero"}, "1": {Text: "one"}, "2": {Text: "two"},
		}}
		chain, _ := NewChain(primary, fallback, MultiOptions{})
		var got []progressRecord
		_, err := chain.TranscribeSegments(context.Background(), segments("0", "1", "2"), recordProgress(&got))
		if err != nil {
			t.Fatal(err)
		}
		want := []progressRecord{
			{kind: "started", index: 0, provider: "primary"},
			{kind: "done", index: 0, provider: "primary"},
			{kind: "started", index: 1, provider: "primary"},
			{kind: "failed", index: 1, provider: "primary", class: errs.ServerError},
			{kind: "started", index: 0, provider: "fallback"},
			{kind: "done", index: 0, provider: "fallback"},
			{kind: "started", index: 1, provider: "fallback"},
			{kind: "done", index: 1, provider: "fallback"},
			{kind: "started", index: 2, provider: "fallback"},
			{kind: "done", index: 2, provider: "fallback"},
		}
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(primary.calls, []string{"0", "1"}) ||
			!reflect.DeepEqual(fallback.calls, []string{"0", "1", "2"}) {
			t.Fatalf("progress=%#v primary=%v fallback=%v", got, primary.calls, fallback.calls)
		}
	})
}

func TestSegmentsMixedProvidersRequireOptInAndAreTraced(t *testing.T) {
	for _, test := range []struct {
		name         string
		failed       string
		wantProvider []string
	}{
		{name: "primary then fallback", failed: "2", wantProvider: []string{"primary", "fallback"}},
		{name: "fallback then primary", failed: "1", wantProvider: []string{"fallback", "primary"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			primary := &fakeProvider{
				name: "primary", responses: map[string]Result{"1": {Text: "one"}, "2": {Text: "two"}},
				errors: map[string]error{test.failed: errs.New(errs.ServerError, "fake", "down")},
			}
			fallback := &fakeProvider{name: "fallback", responses: map[string]Result{
				"1": {Text: "one"}, "2": {Text: "two"},
			}}
			chain, _ := NewChain(primary, fallback, MultiOptions{AllowMixedProviders: true})
			result, err := chain.TranscribeSegments(context.Background(), segments("1", "2"), Progress{})
			if err != nil {
				t.Fatal(err)
			}
			warningsOK := len(result.Warnings) == 2 && result.Warnings[0].Class == errs.ServerError &&
				result.Warnings[0].Provider == "primary" && strings.Contains(result.Warnings[0].Message, "fallback STT") &&
				result.Warnings[1].Class == errs.Unavailable && result.Warnings[1].Message == WarningMixedProviders
			if result.Provider != ProviderMixed || result.Text != "one two" || !warningsOK {
				t.Fatalf("result = %#v", result)
			}
			for index, segment := range result.Segments {
				if segment.Provider != test.wantProvider[index] {
					t.Fatalf("segments = %#v, providers attendus=%v", result.Segments, test.wantProvider)
				}
			}
		})
	}
}

func TestPartialMixedProviderProvenanceIsPreservedInBatchAndStream(t *testing.T) {
	for _, mode := range []string{"batch", "stream"} {
		t.Run(mode, func(t *testing.T) {
			primary := &fakeProvider{
				name: "primary", responses: map[string]Result{"0": {Text: "zero"}},
				errors: map[string]error{
					"1": errs.New(errs.ServerError, "fake", "down"),
					"2": errs.New(errs.ServerError, "fake", "down"),
				},
			}
			fallback := &fakeProvider{
				name: "fallback", responses: map[string]Result{"1": {Text: "one"}},
				errors: map[string]error{"2": errs.New(errs.ServerError, "fake", "down")},
			}
			chain, err := NewChain(primary, fallback, MultiOptions{AllowMixedProviders: true, AllowPartial: true, Workers: 1})
			if err != nil {
				t.Fatal(err)
			}
			var result MultiResult
			if mode == "batch" {
				result, err = chain.TranscribeSegments(context.Background(), segments("0", "1", "2"), Progress{})
			} else {
				input := make(chan Segment, 3)
				for _, segment := range segments("0", "1", "2") {
					input <- segment
				}
				close(input)
				result, err = chain.TranscribeStream(context.Background(), input, Progress{})
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []SegmentResult{
				{Index: 0, Text: "zero", Provider: "primary"},
				{Index: 1, Text: "one", Provider: "fallback"},
			}
			if result.Provider != ProviderMixed || !result.Partial || !reflect.DeepEqual(result.Segments, want) {
				t.Fatalf("result = %#v", result)
			}
			if len(result.Warnings) != 3 || result.Warnings[1].Message != WarningMixedProviders || result.Warnings[2].Message != WarningPartial {
				t.Fatalf("warnings = %#v", result.Warnings)
			}
		})
	}
}

func TestBatchMixedPartialWarningPreservesFailureClassAndProvider(t *testing.T) {
	for _, test := range []struct {
		name         string
		fallback     Provider
		wantClass    errs.Class
		wantProvider string
	}{
		{
			name:         "primary failure",
			wantClass:    errs.AuthFailed,
			wantProvider: "primary",
		},
		{
			name: "fallback failure",
			fallback: &fakeProvider{
				name:   "fallback",
				errors: map[string]error{"1": errs.New(errs.Timeout, "fake", "timeout")},
			},
			wantClass:    errs.Timeout,
			wantProvider: "fallback",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			primary := &fakeProvider{
				name:      "primary",
				responses: map[string]Result{"0": {Text: "zero"}},
				errors:    map[string]error{"1": errs.New(errs.AuthFailed, "fake", "refus")},
			}
			chain, err := NewChain(primary, test.fallback, MultiOptions{AllowMixedProviders: true, AllowPartial: true})
			if err != nil {
				t.Fatal(err)
			}
			result, err := chain.TranscribeSegments(context.Background(), segments("0", "1"), Progress{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != "zero" || !result.Partial || len(result.Warnings) != 1 {
				t.Fatalf("result=%#v", result)
			}
			warning := result.Warnings[0]
			if warning.Message != WarningPartial || warning.Class != test.wantClass || warning.Provider != test.wantProvider {
				t.Fatalf("warning=%#v", warning)
			}
		})
	}
}

func TestDoubleFailurePreservesCausesWithoutLeakingMessages(t *testing.T) {
	primaryErr := errors.New("primary-secret")
	fallbackErr := errors.New("fallback-secret")
	primary := &fakeProvider{name: "primary", errors: map[string]error{"1": primaryErr}}
	fallback := &fakeProvider{name: "fallback", errors: map[string]error{"1": fallbackErr}}
	chain, _ := NewChain(primary, fallback, MultiOptions{})

	_, err := chain.TranscribeSegments(context.Background(), segments("1"), Progress{})
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

func TestFallbackClassWinsOverPrimaryDeadline(t *testing.T) {
	primaryErr := context.DeadlineExceeded
	fallbackErr := errs.New(errs.AuthFailed, "fallback", "authentication failed")
	primary := &fakeProvider{name: "primary", errors: map[string]error{"0": primaryErr}}
	fallback := &fakeProvider{name: "fallback", errors: map[string]error{"0": fallbackErr}}
	chain, err := NewChain(primary, fallback, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = chain.TranscribeSegments(context.Background(), segments("0"), Progress{})
	if errs.Classify(err) != errs.AuthFailed || ProviderFromError(err) != "fallback" {
		t.Fatalf("error=%v class=%s provider=%q", err, errs.Classify(err), ProviderFromError(err))
	}
	if !errors.Is(err, primaryErr) || !errors.Is(err, fallbackErr) {
		t.Fatalf("causes perdues: %v", err)
	}
}

func TestMixedPartialAllSegmentsFailedPreservesEffectiveFallbackError(t *testing.T) {
	for _, mode := range []string{"batch", "stream"} {
		t.Run(mode, func(t *testing.T) {
			primaryErr := errs.New(errs.AuthFailed, "primary", "primary-secret")
			fallbackErr := errs.New(errs.Timeout, "fallback", "fallback-secret")
			primary := &fakeProvider{name: "primary", errors: map[string]error{"0": primaryErr}}
			fallback := &fakeProvider{name: "fallback", errors: map[string]error{"0": fallbackErr}}
			chain, err := NewChain(primary, fallback, MultiOptions{AllowMixedProviders: true, AllowPartial: true})
			if err != nil {
				t.Fatal(err)
			}

			var result MultiResult
			if mode == "batch" {
				result, err = chain.TranscribeSegments(context.Background(), segments("0"), Progress{})
			} else {
				input := make(chan Segment, 1)
				input <- segments("0")[0]
				close(input)
				result, err = chain.TranscribeStream(context.Background(), input, Progress{})
			}
			if errs.Classify(err) != errs.Timeout || ProviderFromError(err) != "fallback" {
				t.Fatalf("result=%#v error=%v class=%s provider=%q", result, err, errs.Classify(err), ProviderFromError(err))
			}
			if !errors.Is(err, primaryErr) || !errors.Is(err, fallbackErr) {
				t.Fatalf("causes perdues: %v", err)
			}
			if strings.Contains(err.Error(), "primary-secret") || strings.Contains(err.Error(), "fallback-secret") {
				t.Fatalf("message sensible exposé: %v", err)
			}
			if result.Text != "" || result.Segments != nil {
				t.Fatalf("partial result leaked: %#v", result)
			}
		})
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

func TestSegmentTextBudgetCountsUTF8BytesAndRejectsPartialOutput(t *testing.T) {
	provider := &fakeProvider{name: "primary", responses: map[string]Result{
		"0": {Text: "éé"},
		"1": {Text: "éé"},
	}}
	chain, err := NewChain(provider, nil, MultiOptions{AllowPartial: true, MaxTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	result, err := chain.TranscribeSegments(context.Background(), segments("0", "1"), Progress{})
	if errs.Classify(err) != errs.ServerError || err.Error() != "stt: "+TextBudgetExceeded {
		t.Fatalf("error=%v class=%s", err, errs.Classify(err))
	}
	if provider := ProviderFromError(err); provider != "primary" {
		t.Fatalf("provider=%q", provider)
	}
	if result.Text != "" || result.Segments != nil {
		t.Fatalf("partial result leaked: %#v", result)
	}
}

func TestBatchFatalErrorsOverrideEarlierRecoverableFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		firstFailure bool
		allowPartial bool
		text         string
		limit        int
	}{
		{name: "partial ASCII separator", firstFailure: true, allowPartial: true, text: "aaaa", limit: 8},
		{name: "strict ASCII separator", text: "aaaa", limit: 8},
		{name: "partial UTF-8 bytes", firstFailure: true, allowPartial: true, text: "éé", limit: 8},
		{name: "strict UTF-8 bytes", text: "éé", limit: 8},
		{name: "no earlier failure", allowPartial: true, text: "aaaa", limit: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeProvider{name: "primary", responses: map[string]Result{
				"1": {Text: tc.text}, "2": {Text: tc.text},
			}}
			paths := []string{"1", "2"}
			if tc.firstFailure {
				provider.errors = map[string]error{"0": errs.New(errs.Unavailable, "fake", "down")}
				paths = append([]string{"0"}, paths...)
			}
			chain, err := NewChain(provider, nil, MultiOptions{AllowPartial: tc.allowPartial, MaxTextBytes: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			result, err := chain.TranscribeSegments(context.Background(), segments(paths...), Progress{})
			if !IsLimitExceeded(err) || errs.Classify(err) != errs.ServerError || ProviderFromError(err) != "primary" || result.Text != "" || result.Segments != nil {
				t.Fatalf("result=%#v error=%v provider=%q", result, err, ProviderFromError(err))
			}
		})
	}
}

func TestBatchCancellationOverridesEarlierRecoverableFailure(t *testing.T) {
	for _, allowPartial := range []bool{false, true} {
		t.Run(strconv.FormatBool(allowPartial), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := providerFunc{name: "primary", transcribe: func(_ context.Context, request Request) (Result, error) {
				if request.Model == "0" {
					return Result{}, errs.New(errs.Unavailable, "fake", "down")
				}
				cancel()
				return Result{Text: "ok"}, nil
			}}
			chain, err := NewChain(provider, nil, MultiOptions{AllowPartial: allowPartial})
			if err != nil {
				t.Fatal(err)
			}
			result, err := chain.TranscribeSegments(ctx, segments("0", "1", "2"), Progress{})
			want := errs.Cancelled
			if !allowPartial {
				want = errs.Unavailable // Strict mode stops on the first error.
			}
			if errs.Classify(err) != want || result.Text != "" || result.Segments != nil {
				t.Fatalf("result=%#v error=%v", result, err)
			}
		})
	}
}

func TestFallbackSharesTextBudgetWithDiscardedPrimaryResults(t *testing.T) {
	primary := &fakeProvider{
		name:      "primary",
		responses: map[string]Result{"0": {Text: "aaaa"}},
		errors:    map[string]error{"1": errs.New(errs.ServerError, "fake", "down")},
	}
	fallback := &fakeProvider{name: "fallback", responses: map[string]Result{
		"0": {Text: "bbbb"},
		"1": {Text: "cccc"},
	}}
	chain, err := NewChain(primary, fallback, MultiOptions{MaxTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	result, err := chain.TranscribeSegments(context.Background(), segments("0", "1"), Progress{})
	if errs.Classify(err) != errs.ServerError || err.Error() != "stt: "+TextBudgetExceeded {
		t.Fatalf("error=%v class=%s", err, errs.Classify(err))
	}
	if provider := ProviderFromError(err); provider != "fallback" {
		t.Fatalf("provider=%q", provider)
	}
	if result.Text != "" || result.Segments != nil {
		t.Fatalf("partial result leaked: %#v", result)
	}
	if !reflect.DeepEqual(fallback.calls, []string{"0"}) {
		t.Fatalf("fallback calls=%v", fallback.calls)
	}
}

func TestStreamFallbackBudgetReportsFallbackProvider(t *testing.T) {
	primary := &fakeProvider{
		name:      "primary",
		responses: map[string]Result{"0": {Text: "aaaa"}},
		errors:    map[string]error{"1": errs.New(errs.ServerError, "fake", "down")},
	}
	fallback := &fakeProvider{name: "fallback", responses: map[string]Result{
		"0": {Text: "bbbb"},
		"1": {Text: "cccc"},
	}}
	chain, err := NewChain(primary, fallback, MultiOptions{Workers: 1, MaxTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan Segment, 2)
	for _, segment := range segments("0", "1") {
		input <- segment
	}
	close(input)
	result, err := chain.TranscribeStream(context.Background(), input, Progress{})
	if !IsLimitExceeded(err) || ProviderFromError(err) != "fallback" {
		t.Fatalf("result=%#v error=%v provider=%q", result, err, ProviderFromError(err))
	}
	if result.Text != "" || result.Segments != nil {
		t.Fatalf("partial result leaked: %#v", result)
	}
}

func TestStreamTextBudgetCancelsInflightAndDrainsQueuedRequests(t *testing.T) {
	large := strings.Repeat("é", (MaxResponseBytes-1024)/2) // Just under 4 MiB in UTF-8.
	blockerStarted := make(chan struct{})
	var blockerOnce sync.Once
	var calls, active, cancelled atomic.Int32
	provider := providerFunc{name: "primary", transcribe: func(ctx context.Context, request Request) (Result, error) {
		calls.Add(1)
		active.Add(1)
		defer active.Add(-1)
		switch request.Model {
		case "0":
			return Result{Text: large}, nil
		case "1":
			<-blockerStarted
			return Result{Text: large}, nil
		default:
			blockerOnce.Do(func() { close(blockerStarted) })
			<-ctx.Done()
			cancelled.Add(1)
			return Result{}, ctx.Err()
		}
	}}
	chain, err := NewChain(provider, nil, MultiOptions{Workers: 64, MaxTextBytes: 6 << 20})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan Segment, 32)
	for index := range 32 {
		input <- Segment{Index: index, Request: Request{Model: strconv.Itoa(index)}}
	}
	close(input)
	result, err := chain.TranscribeStream(context.Background(), input, Progress{})
	if errs.Classify(err) != errs.ServerError || err.Error() != "stt: "+TextBudgetExceeded {
		t.Fatalf("error=%v class=%s", err, errs.Classify(err))
	}
	if provider := ProviderFromError(err); provider != "primary" {
		t.Fatalf("provider=%q", provider)
	}
	if result.Text != "" || result.Segments != nil {
		t.Fatalf("partial result leaked: %#v", result)
	}
	if calls.Load() != 3 || cancelled.Load() != 1 || active.Load() != 0 {
		t.Fatalf("calls=%d cancelled=%d active=%d", calls.Load(), cancelled.Load(), active.Load())
	}
}

func TestStreamSegmentLimitBoundsCollection(t *testing.T) {
	var calls atomic.Int32
	provider := providerFunc{name: "primary", transcribe: func(_ context.Context, _ Request) (Result, error) {
		calls.Add(1)
		return Result{Text: "ok"}, nil
	}}
	chain, err := NewChain(provider, nil, MultiOptions{Workers: 2, MaxSegments: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan Segment, 4)
	for index := range 4 {
		input <- Segment{Index: index}
	}
	close(input)
	result, err := chain.TranscribeStream(context.Background(), input, Progress{})
	if errs.Classify(err) != errs.ServerError || err.Error() != "stt: "+SegmentLimitExceeded {
		t.Fatalf("error=%v class=%s", err, errs.Classify(err))
	}
	if provider := ProviderFromError(err); provider != "primary" {
		t.Fatalf("provider=%q", provider)
	}
	if result.Text != "" || result.Segments != nil || calls.Load() > 2 {
		t.Fatalf("result=%#v calls=%d", result, calls.Load())
	}
}

func TestStreamLimitDoesNotDrainAnOpenInputChannel(t *testing.T) {
	provider := providerFunc{name: "primary", transcribe: func(context.Context, Request) (Result, error) {
		return Result{Text: "ok"}, nil
	}}
	chain, err := NewChain(provider, nil, MultiOptions{MaxSegments: 1})
	if err != nil {
		t.Fatal(err)
	}
	input := make(chan Segment)
	thirdSent := make(chan struct{})
	producerDone := make(chan struct{})
	go func() {
		input <- Segment{Index: 0}
		input <- Segment{Index: 1}
		input <- Segment{Index: 2}
		close(thirdSent)
		close(input)
		close(producerDone)
	}()
	result := make(chan error, 1)
	go func() {
		_, err := chain.TranscribeStream(context.Background(), input, Progress{})
		result <- err
	}()
	select {
	case err := <-result:
		if !IsLimitExceeded(err) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("TranscribeStream did not return after the segment limit")
	}
	select {
	case <-thirdSent:
		t.Fatal("TranscribeStream kept draining input after returning")
	case <-time.After(50 * time.Millisecond):
	}
	<-input
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("producer did not stop after its blocked send was released")
	}
}

type providerFunc struct {
	name       string
	transcribe func(context.Context, Request) (Result, error)
}

type progressRecord struct {
	kind     string
	index    int
	provider string
	class    errs.Class
}

func recordProgress(records *[]progressRecord) Progress {
	return Progress{
		Started: func(index int, provider string) {
			*records = append(*records, progressRecord{kind: "started", index: index, provider: provider})
		},
		Done: func(index int, provider string) {
			*records = append(*records, progressRecord{kind: "done", index: index, provider: provider})
		},
		Failed: func(index int, provider string, class errs.Class) {
			*records = append(*records, progressRecord{kind: "failed", index: index, provider: provider, class: class})
		},
	}
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
