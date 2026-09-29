package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/events"
	"git.2027a.net/2027a/murmur/internal/policy"
)

// Provider is the common contract implemented by every STT backend.
type Provider interface {
	Name() string
	Transcribe(context.Context, Request) (Result, error)
}

// HealthChecker is implemented by providers with a configured health endpoint.
type HealthChecker interface {
	Health(context.Context) error
}

type Request struct {
	Audio    wav.File
	Filename string
	Model    string
	Language string
	Prompt   string
}

type Result struct {
	Text     string
	Provider string
	Raw      json.RawMessage
	Warnings []Warning
}

type Config struct {
	Name            string
	BaseURL         string
	Model           string
	Language        string
	Prompt          string
	APIKey          string
	Timeout         time.Duration
	AllowedZone     policy.Zone
	TrustedNetworks []netip.Prefix
	AllowInsecure   bool
	HealthEnabled   bool
	HealthEndpoint  string
	Retries         int
	RetryBackoff    time.Duration
}

type Segment struct {
	Index    int
	Request  Request
	QueuedAt time.Time
	Overlap  bool
}

type SegmentResult struct {
	Index        int
	Text         string
	Provider     string
	Overlap      bool
	OverlapWords int
}

type MultiResult struct {
	Text     string
	Provider string
	Segments []SegmentResult
	Warnings []Warning
	Partial  bool
}

type Warning struct {
	Class     errs.Class
	Provider  string
	Component string
	Message   string
}

type MultiOptions struct {
	AllowMixedProviders bool
	AllowPartial        bool
	Workers             int
	MaxTextBytes        int
	MaxSegments         int
}

type Progress struct {
	// Indexes are the original zero-based Segment.Index values.
	Started func(index int, provider string)
	Done    func(index int, provider string)
	Failed  func(index int, provider string, class errs.Class)
}

const (
	ProviderMixed         = "mixed"
	WarningMixedProviders = "transcription mixte: plusieurs providers ont été utilisés et sont tracés par segment"
	WarningPartial        = "transcription partielle: au moins un segment a échoué et a été omis"
	TextBudgetExceeded    = "volume cumulé des transcriptions supérieur à la limite configurée"
	SegmentLimitExceeded  = "nombre de segments STT supérieur à la limite configurée"
	DefaultMaxTextBytes   = 16 << 20
	DefaultMaxSegments    = 4096
	MaximumMaxTextBytes   = 64 << 20
	MaximumMaxSegments    = 65536
)

type transcriptLimitError struct {
	cause    *errs.Error
	provider string
}

func (e *transcriptLimitError) Error() string { return e.cause.Error() }
func (e *transcriptLimitError) Unwrap() error { return e.cause }

func newTranscriptLimitError(message, provider string) error {
	return &transcriptLimitError{cause: errs.New(errs.ServerError, "stt", message), provider: provider}
}

func isTranscriptLimitError(err error) bool {
	var limit *transcriptLimitError
	return errors.As(err, &limit)
}

// IsLimitExceeded reports resource limits that must never enter fallback or delivery.
func IsLimitExceeded(err error) bool { return isTranscriptLimitError(err) }

type textBudget struct {
	mu      sync.Mutex
	limit   int
	used    int
	entries int
	err     error
}

func newTextBudget(limit int) *textBudget {
	return &textBudget{limit: limit}
}

func (b *textBudget) add(text, provider string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	remaining := b.limit - b.used
	if b.entries > 0 {
		if remaining == 0 {
			b.err = newTranscriptLimitError(TextBudgetExceeded, provider)
			return b.err
		}
		remaining--
	}
	if len(text) > remaining {
		b.err = newTranscriptLimitError(TextBudgetExceeded, provider)
		return b.err
	}
	b.used += len(text)
	if b.entries > 0 {
		b.used++
	}
	b.entries++
	return nil
}

// Chain applies a primary/fallback policy to one or more replayable audio files.
type Chain struct {
	primary  Provider
	fallback Provider
	options  MultiOptions
}

func NewChain(primary, fallback Provider, options MultiOptions) (*Chain, error) {
	if primary == nil {
		return nil, errs.New(errs.ConfigInvalid, "stt", "provider principal manquant")
	}
	if fallback != nil && fallback.Name() == primary.Name() {
		return nil, errs.New(errs.ConfigInvalid, "stt", "le fallback doit être distinct du provider principal")
	}
	if options.Workers < 0 {
		return nil, errs.New(errs.ConfigInvalid, "stt", "le nombre de workers ne peut pas être négatif")
	}
	if options.Workers == 0 {
		options.Workers = 1
	}
	if options.MaxTextBytes < 0 || options.MaxTextBytes > MaximumMaxTextBytes {
		return nil, errs.New(errs.ConfigInvalid, "stt", "le budget de texte doit être inférieur ou égal à 67108864")
	}
	if options.MaxTextBytes == 0 {
		options.MaxTextBytes = DefaultMaxTextBytes
	}
	if options.MaxSegments < 0 || options.MaxSegments > MaximumMaxSegments {
		return nil, errs.New(errs.ConfigInvalid, "stt", "le nombre maximal de segments doit être inférieur ou égal à 65536")
	}
	if options.MaxSegments == 0 {
		options.MaxSegments = DefaultMaxSegments
	}
	responseSlots := options.MaxTextBytes / MaxResponseBytes
	if options.MaxTextBytes%MaxResponseBytes != 0 {
		responseSlots++
	}
	if responseSlots < 1 {
		responseSlots = 1
	}
	if options.Workers > responseSlots {
		options.Workers = responseSlots
	}
	return &Chain{primary: primary, fallback: fallback, options: options}, nil
}

func (c *Chain) Name() string { return c.primary.Name() }

func (c *Chain) ValidateSegmentCount(count int) error {
	if count > c.options.MaxSegments {
		return newTranscriptLimitError(SegmentLimitExceeded, c.primary.Name())
	}
	return nil
}

func (c *Chain) Transcribe(ctx context.Context, request Request) (Result, error) {
	budget := newTextBudget(c.options.MaxTextBytes)
	result, primaryErr := invoke(ctx, c.primary, request)
	if primaryErr == nil {
		if err := budget.add(result.Text, c.primary.Name()); err != nil {
			return Result{}, err
		}
		return result, nil
	}
	if c.fallback == nil || errs.Classify(primaryErr) == errs.Cancelled {
		return Result{}, failedAttempt(c.primary, primaryErr)
	}
	result, fallbackErr := invoke(ctx, c.fallback, request)
	if fallbackErr != nil {
		return Result{}, failedAttempts(c.primary, c.fallback, primaryErr, fallbackErr)
	}
	if err := budget.add(result.Text, c.fallback.Name()); err != nil {
		return Result{}, err
	}
	result.Warnings = append(result.Warnings, fallbackWarning(c.primary, primaryErr))
	return result, nil
}

func (c *Chain) TranscribeSegments(ctx context.Context, segments []Segment, progress Progress) (MultiResult, error) {
	if err := c.ValidateSegmentCount(len(segments)); err != nil {
		return MultiResult{}, err
	}
	ordered, err := validateSegments(segments)
	if err != nil {
		return MultiResult{}, err
	}
	queuedAt := time.Now()
	for i := range ordered {
		if ordered[i].QueuedAt.IsZero() {
			ordered[i].QueuedAt = queuedAt
		}
	}
	if c.options.Workers > 1 {
		input := make(chan Segment, len(ordered))
		for _, segment := range ordered {
			input <- segment
		}
		close(input)
		return c.TranscribeStream(ctx, input, progress)
	}
	if c.options.AllowMixedProviders {
		return c.transcribeMixed(ctx, ordered, progress, newTextBudget(c.options.MaxTextBytes))
	}

	budget := newTextBudget(c.options.MaxTextBytes)
	results, primaryErr := transcribeWith(ctx, c.primary, ordered, c.options.AllowPartial && c.fallback == nil, progress, budget)
	if primaryErr == nil {
		return assemble(results, len(ordered), nil), nil
	}
	if errs.Classify(primaryErr) == errs.Cancelled {
		return MultiResult{}, primaryErr
	}
	if isTranscriptLimitError(primaryErr) {
		return MultiResult{}, primaryErr
	}
	if c.fallback == nil {
		if c.options.AllowPartial && len(results) > 0 {
			return assemble(results, len(ordered), []Warning{partialWarning(c.primary, primaryErr)}), nil
		}
		return MultiResult{}, failedAttempt(c.primary, primaryErr)
	}

	results = nil
	results, fallbackErr := transcribeWith(ctx, c.fallback, ordered, c.options.AllowPartial, progress, budget)
	if isTranscriptLimitError(fallbackErr) {
		return MultiResult{}, fallbackErr
	}
	if fallbackErr == nil {
		return assemble(results, len(ordered), []Warning{fallbackWarning(c.primary, primaryErr)}), nil
	}
	if c.options.AllowPartial && len(results) > 0 && errs.Classify(fallbackErr) != errs.Cancelled {
		return assemble(results, len(ordered), []Warning{
			fallbackWarning(c.primary, primaryErr),
			partialWarning(c.fallback, fallbackErr),
		}), nil
	}
	return MultiResult{}, failedAttempts(c.primary, c.fallback, primaryErr, fallbackErr)
}

// TranscribeStream consumes segments while they are produced. Provider calls
// are bounded by Workers and final results are always assembled by segment index.
// The caller retains ownership of input and must stop its producer after return;
// the chain never detaches a drain goroutine.
func (c *Chain) TranscribeStream(ctx context.Context, input <-chan Segment, progress Progress) (MultiResult, error) {
	budget := newTextBudget(c.options.MaxTextBytes)
	segments, primaryResults, primaryErr := transcribeStreamWith(ctx, c.primary, input, c.options.Workers, c.options.MaxSegments, progress, budget)
	if len(segments) == 0 {
		if primaryErr != nil {
			return MultiResult{}, primaryErr
		}
		return MultiResult{}, errs.New(errs.EmptyResponse, "stt", "aucun segment à transcrire")
	}
	ordered, err := validateSegments(segments)
	if err != nil {
		return MultiResult{}, err
	}
	if primaryErr == nil {
		return assemble(orderedResults(primaryResults), len(ordered), nil), nil
	}
	if errs.Classify(primaryErr) == errs.Cancelled {
		return MultiResult{}, primaryErr
	}
	if isTranscriptLimitError(primaryErr) {
		return MultiResult{}, primaryErr
	}
	if c.options.AllowMixedProviders {
		return c.finishMixedStream(ctx, ordered, primaryResults, primaryErr, progress, budget)
	}
	if c.fallback == nil {
		if c.options.AllowPartial && len(primaryResults) > 0 {
			return assemble(orderedResults(primaryResults), len(ordered), []Warning{partialWarning(c.primary, primaryErr)}), nil
		}
		return MultiResult{}, failedAttempt(c.primary, primaryErr)
	}
	primaryResults = nil
	fallbackResults, fallbackErr := transcribeBatchWith(ctx, c.fallback, ordered, c.options.Workers, progress, budget)
	if isTranscriptLimitError(fallbackErr) {
		return MultiResult{}, fallbackErr
	}
	if fallbackErr == nil {
		return assemble(orderedResults(fallbackResults), len(ordered), []Warning{fallbackWarning(c.primary, primaryErr)}), nil
	}
	if c.options.AllowPartial && len(fallbackResults) > 0 && errs.Classify(fallbackErr) != errs.Cancelled {
		return assemble(orderedResults(fallbackResults), len(ordered), []Warning{
			fallbackWarning(c.primary, primaryErr), partialWarning(c.fallback, fallbackErr),
		}), nil
	}
	return MultiResult{}, failedAttempts(c.primary, c.fallback, primaryErr, fallbackErr)
}

func (c *Chain) finishMixedStream(ctx context.Context, segments []Segment, primary map[int]SegmentResult, primaryErr error, progress Progress, budget *textBudget) (MultiResult, error) {
	if c.fallback == nil {
		if c.options.AllowPartial && len(primary) > 0 {
			return assemble(orderedResults(primary), len(segments), []Warning{partialWarning(c.primary, primaryErr)}), nil
		}
		return MultiResult{}, failedAttempt(c.primary, primaryErr)
	}
	failed := make([]Segment, 0, len(segments)-len(primary))
	for _, segment := range segments {
		if _, ok := primary[segment.Index]; !ok {
			failed = append(failed, segment)
		}
	}
	fallback, fallbackErr := transcribeBatchWith(ctx, c.fallback, failed, c.options.Workers, progress, budget)
	if isTranscriptLimitError(fallbackErr) {
		return MultiResult{}, fallbackErr
	}
	if fallbackErr != nil && (!c.options.AllowPartial || errs.Classify(fallbackErr) == errs.Cancelled) {
		return MultiResult{}, failedAttempts(c.primary, c.fallback, primaryErr, fallbackErr)
	}
	for index, result := range fallback {
		primary[index] = result
	}
	warnings := []Warning{fallbackWarning(c.primary, primaryErr)}
	providers := make(map[string]struct{})
	for _, result := range primary {
		providers[result.Provider] = struct{}{}
	}
	if len(providers) > 1 {
		warnings = append(warnings, Warning{Class: errs.Unavailable, Component: "stt", Message: WarningMixedProviders})
	}
	if fallbackErr != nil {
		warnings = append(warnings, partialWarning(c.fallback, fallbackErr))
	}
	if len(primary) == 0 {
		return MultiResult{}, failedAttempts(c.primary, c.fallback, primaryErr, fallbackErr)
	}
	return assemble(orderedResults(primary), len(segments), warnings), nil
}

type streamOutcome struct {
	result SegmentResult
	err    error
}

func transcribeStreamWith(ctx context.Context, provider Provider, input <-chan Segment, workers, maxSegments int, progress Progress, budget *textBudget) ([]Segment, map[int]SegmentResult, error) {
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan Segment)
	var workerGroup sync.WaitGroup
	var mu sync.Mutex
	results := make(map[int]SegmentResult)
	var firstErr error
	firstErrIndex := 0
	workerGroup.Add(workers)
	for range workers {
		go func() {
			defer workerGroup.Done()
			for {
				var segment Segment
				var ok bool
				select {
				case <-workCtx.Done():
					return
				case segment, ok = <-jobs:
					if !ok {
						return
					}
				}
				if err := workCtx.Err(); err != nil {
					return
				}
				outcome := transcribeOne(workCtx, provider, segment, progress, budget)
				mu.Lock()
				if outcome.err == nil {
					results[segment.Index] = outcome.result
				} else if firstErr == nil || segment.Index < firstErrIndex {
					firstErr = outcome.err
					firstErrIndex = segment.Index
				}
				mu.Unlock()
				if isTranscriptLimitError(outcome.err) {
					cancel(outcome.err)
				}
			}
		}()
	}
	var segments []Segment
consume:
	for {
		select {
		case <-workCtx.Done():
			break consume
		case segment, ok := <-input:
			if !ok {
				break consume
			}
			if len(segments) == maxSegments {
				cancel(newTranscriptLimitError(SegmentLimitExceeded, provider.Name()))
				break consume
			}
			if segment.QueuedAt.IsZero() {
				segment.QueuedAt = time.Now()
			}
			segments = append(segments, segment)
			select {
			case jobs <- segment:
			case <-workCtx.Done():
				break consume
			}
		}
	}
	close(jobs)
	workerGroup.Wait()
	if cause := context.Cause(workCtx); isTranscriptLimitError(cause) {
		return segments, nil, cause
	}
	if err := ctx.Err(); err != nil {
		return segments, nil, err
	}
	return segments, results, firstErr
}

func transcribeBatchWith(ctx context.Context, provider Provider, segments []Segment, workers int, progress Progress, budget *textBudget) (map[int]SegmentResult, error) {
	input := make(chan Segment, len(segments))
	queuedAt := time.Now()
	for _, segment := range segments {
		segment.QueuedAt = queuedAt
		input <- segment
	}
	close(input)
	_, results, err := transcribeStreamWith(ctx, provider, input, workers, len(segments), progress, budget)
	return results, err
}

func transcribeOne(ctx context.Context, provider Provider, segment Segment, progress Progress, budget *textBudget) streamOutcome {
	if err := ctx.Err(); err != nil {
		return streamOutcome{err: err}
	}
	started := time.Now()
	queueMS := int64(0)
	if !segment.QueuedAt.IsZero() {
		queueMS = max(int64(0), started.Sub(segment.QueuedAt).Milliseconds())
	}
	defer func() {
		events.ReportTiming(ctx, events.Timing{
			Stage: "stt", DurationMS: time.Since(started).Milliseconds(), QueueMS: queueMS,
			AudioMS: segment.Request.Audio.Duration().Milliseconds(),
		}, provider.Name(), segment.Index+1)
	}()
	if progress.Started != nil {
		progress.Started(segment.Index, provider.Name())
	}
	result, err := invoke(ctx, provider, segment.Request)
	if err == nil {
		err = budget.add(result.Text, provider.Name())
	}
	if err != nil {
		if progress.Failed != nil {
			progress.Failed(segment.Index, provider.Name(), errs.Classify(err))
		}
		return streamOutcome{err: err}
	}
	if progress.Done != nil {
		progress.Done(segment.Index, result.Provider)
	}
	return streamOutcome{result: SegmentResult{Index: segment.Index, Text: result.Text, Provider: result.Provider, Overlap: segment.Overlap}}
}

func orderedResults(results map[int]SegmentResult) []SegmentResult {
	ordered := make([]SegmentResult, 0, len(results))
	for _, result := range results {
		ordered = append(ordered, result)
	}
	sortSegmentResults(ordered)
	return ordered
}

func (c *Chain) transcribeMixed(ctx context.Context, segments []Segment, progress Progress, budget *textBudget) (MultiResult, error) {
	results := make([]SegmentResult, 0, len(segments))
	providers := make(map[string]struct{})
	partial := false
	var partialProvider Provider
	var partialErr error
	var warnings []Warning
	fallbackUsed := false
	var firstPrimaryErr, firstFallbackErr error
	for _, segment := range segments {
		failedProvider := c.primary
		outcome := transcribeOne(ctx, c.primary, segment, progress, budget)
		err := outcome.err
		if isTranscriptLimitError(err) {
			return MultiResult{}, err
		}
		var primaryErr error
		if err != nil && c.fallback != nil && errs.Classify(err) != errs.Cancelled {
			primaryErr = err
			if firstPrimaryErr == nil {
				firstPrimaryErr = primaryErr
			}
			failedProvider = c.fallback
			segment.QueuedAt = time.Now()
			outcome = transcribeOne(ctx, c.fallback, segment, progress, budget)
			err = outcome.err
			if isTranscriptLimitError(err) {
				return MultiResult{}, err
			}
			if err != nil && firstFallbackErr == nil {
				firstFallbackErr = err
			}
			if err == nil && !fallbackUsed {
				warnings = append(warnings, fallbackWarning(c.primary, primaryErr))
				fallbackUsed = true
			} else if err != nil && (errs.Classify(err) == errs.Cancelled || !c.options.AllowPartial) {
				return MultiResult{}, failedAttempts(c.primary, c.fallback, primaryErr, err)
			}
		}
		if err != nil {
			if errs.Classify(err) == errs.Cancelled || !c.options.AllowPartial {
				if primaryErr != nil && c.fallback != nil {
					return MultiResult{}, failedAttempts(c.primary, c.fallback, primaryErr, err)
				}
				return MultiResult{}, failedAttempt(failedProvider, err)
			}
			partial = true
			if partialErr == nil {
				partialProvider = failedProvider
				partialErr = err
			}
			continue
		}
		providers[outcome.result.Provider] = struct{}{}
		results = append(results, outcome.result)
	}
	if len(results) == 0 {
		if firstPrimaryErr != nil && firstFallbackErr != nil {
			return MultiResult{}, failedAttempts(c.primary, c.fallback, firstPrimaryErr, firstFallbackErr)
		}
		if partialErr != nil && partialProvider != nil {
			return MultiResult{}, failedAttempt(partialProvider, partialErr)
		}
		return MultiResult{}, errs.New(errs.EmptyResponse, "stt", "aucun segment n'a produit de transcription")
	}
	if len(providers) > 1 {
		warnings = append(warnings, Warning{Class: errs.Unavailable, Component: "stt", Message: WarningMixedProviders})
	}
	if partial {
		warnings = append(warnings, partialWarning(partialProvider, partialErr))
	}
	return assemble(results, len(segments), warnings), nil
}

func transcribeWith(ctx context.Context, provider Provider, segments []Segment, continueOnError bool, progress Progress, budget *textBudget) ([]SegmentResult, error) {
	results := make([]SegmentResult, 0, len(segments))
	var firstErr error
	queuedAt := time.Now()
	for _, segment := range segments {
		segment.QueuedAt = queuedAt
		outcome := transcribeOne(ctx, provider, segment, progress, budget)
		err := outcome.err
		if err != nil {
			// A fatal error must not be replaced by an earlier recoverable one.
			if errs.Classify(err) == errs.Cancelled || isTranscriptLimitError(err) {
				return results, err
			}
			if !continueOnError {
				return results, err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results = append(results, outcome.result)
	}
	return results, firstErr
}

func invoke(ctx context.Context, provider Provider, request Request) (Result, error) {
	result, err := provider.Transcribe(ctx, request)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(result.Text) == "" {
		return Result{}, errs.New(errs.EmptyResponse, "stt", fmt.Sprintf("le provider %q n'a renvoyé aucun texte", provider.Name()))
	}
	result.Provider = provider.Name()
	return result, nil
}

func validateSegments(segments []Segment) ([]Segment, error) {
	if len(segments) == 0 {
		return nil, errs.New(errs.ConfigInvalid, "stt", "aucun segment à transcrire")
	}
	ordered := append([]Segment(nil), segments...)
	for _, segment := range ordered {
		if segment.Index < 0 {
			return nil, errs.New(errs.ConfigInvalid, "stt", fmt.Sprintf("index de segment négatif: %d", segment.Index))
		}
	}
	sortSegments(ordered)
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Index == ordered[i].Index {
			return nil, errs.New(errs.ConfigInvalid, "stt", fmt.Sprintf("index de segment dupliqué: %d", ordered[i].Index))
		}
	}
	return ordered, nil
}

func sortSegments(segments []Segment) {
	sort.Slice(segments, func(i, j int) bool { return segments[i].Index < segments[j].Index })
}

func sortSegmentResults(results []SegmentResult) {
	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
}

func assemble(results []SegmentResult, total int, warnings []Warning) MultiResult {
	results = append([]SegmentResult(nil), results...)
	parts := make([]string, 0, len(results))
	providers := make(map[string]struct{})
	for i := range results {
		result := &results[i]
		if i > 0 && result.Overlap && results[i-1].Index+1 == result.Index {
			result.OverlapWords = matchingOverlap(results[i-1].Text, result.Text)
		}
		if text := TrimOverlap(result.Text, result.OverlapWords); text != "" {
			parts = append(parts, text)
		}
		providers[result.Provider] = struct{}{}
	}
	provider := ""
	if len(providers) > 1 {
		provider = ProviderMixed
	} else if len(providers) == 1 {
		for name := range providers {
			provider = name
		}
	}
	return MultiResult{
		Text:     strings.Join(parts, " "),
		Provider: provider,
		Segments: append([]SegmentResult(nil), results...),
		Warnings: append([]Warning(nil), warnings...),
		Partial:  len(results) != total,
	}
}

func fallbackWarning(provider Provider, err error) Warning {
	return Warning{
		Class:     errs.Classify(err),
		Provider:  provider.Name(),
		Component: "stt",
		Message:   fmt.Sprintf("fallback STT utilisé après l'échec du provider %q", provider.Name()),
	}
}

func partialWarning(provider Provider, err error) Warning {
	return Warning{Class: errs.Classify(err), Provider: provider.Name(), Component: "stt", Message: WarningPartial}
}

type providerAttemptsError struct {
	effective string
	class     errs.Class
	causes    []error
}

func (e *providerAttemptsError) Error() string              { return "échec d'un ou plusieurs providers STT" }
func (e *providerAttemptsError) Unwrap() []error            { return e.causes }
func (e *providerAttemptsError) EffectiveClass() errs.Class { return e.class }

func failedAttempt(provider Provider, err error) error {
	class := errs.Classify(err)
	cause := &providerAttemptsError{effective: provider.Name(), class: class, causes: []error{err}}
	return errs.Wrap(class, "stt", fmt.Sprintf("provider %q en échec (%s)", provider.Name(), class), cause)
}

func failedAttempts(primary, fallback Provider, primaryErr, fallbackErr error) error {
	class := errs.Classify(fallbackErr)
	cause := &providerAttemptsError{effective: fallback.Name(), class: class, causes: []error{primaryErr, fallbackErr}}
	message := fmt.Sprintf(
		"providers %q (%s) et %q (%s) en échec",
		primary.Name(), errs.Classify(primaryErr), fallback.Name(), errs.Classify(fallbackErr),
	)
	return errs.Wrap(class, "stt", message, cause)
}

func ProviderFromError(err error) string {
	var limit *transcriptLimitError
	if errors.As(err, &limit) {
		return limit.provider
	}
	var attempts *providerAttemptsError
	if errors.As(err, &attempts) {
		return attempts.effective
	}
	return ""
}
