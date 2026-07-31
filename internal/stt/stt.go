package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/policy"
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
	HealthEnabled   bool
	HealthEndpoint  string
	Retries         int
	RetryBackoff    time.Duration
}

type Segment struct {
	Index   int
	Request Request
}

type SegmentResult struct {
	Index    int
	Text     string
	Provider string
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
}

type Progress struct {
	Started func(index int, provider string)
	Done    func(index int, provider string)
	Failed  func(index int, provider string, class errs.Class)
}

const (
	WarningMixedProviders = "transcription mixte: plusieurs providers ont été utilisés et sont tracés par segment"
	WarningPartial        = "transcription partielle: au moins un segment a échoué et a été omis"
)

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
	return &Chain{primary: primary, fallback: fallback, options: options}, nil
}

func (c *Chain) Name() string { return c.primary.Name() }

func (c *Chain) Transcribe(ctx context.Context, request Request) (Result, error) {
	result, primaryErr := invoke(ctx, c.primary, request)
	if primaryErr == nil {
		return result, nil
	}
	if c.fallback == nil || errs.Classify(primaryErr) == errs.Cancelled {
		return Result{}, failedAttempt(c.primary, primaryErr)
	}
	result, fallbackErr := invoke(ctx, c.fallback, request)
	if fallbackErr != nil {
		return Result{}, failedAttempts(c.primary, c.fallback, primaryErr, fallbackErr)
	}
	result.Warnings = append(result.Warnings, fallbackWarning(c.primary, primaryErr))
	return result, nil
}

func (c *Chain) TranscribeSegments(ctx context.Context, segments []Segment) (MultiResult, error) {
	ordered, err := validateSegments(segments)
	if err != nil {
		return MultiResult{}, err
	}
	if c.options.AllowMixedProviders {
		return c.transcribeMixed(ctx, ordered)
	}

	results, primaryErr := transcribeWith(ctx, c.primary, ordered, c.options.AllowPartial && c.fallback == nil)
	if primaryErr == nil {
		return assemble(results, len(ordered), nil), nil
	}
	if errs.Classify(primaryErr) == errs.Cancelled {
		return MultiResult{}, primaryErr
	}
	if c.fallback == nil {
		if c.options.AllowPartial && len(results) > 0 {
			return assemble(results, len(ordered), []Warning{partialWarning(c.primary, primaryErr)}), nil
		}
		return MultiResult{}, failedAttempt(c.primary, primaryErr)
	}

	results, fallbackErr := transcribeWith(ctx, c.fallback, ordered, c.options.AllowPartial)
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
func (c *Chain) TranscribeStream(ctx context.Context, input <-chan Segment, progress Progress) (MultiResult, error) {
	segments, primaryResults, primaryErr := transcribeStreamWith(ctx, c.primary, input, c.options.Workers, progress)
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
	if c.options.AllowMixedProviders {
		return c.finishMixedStream(ctx, ordered, primaryResults, primaryErr, progress)
	}
	if c.fallback == nil {
		if c.options.AllowPartial && len(primaryResults) > 0 {
			return assemble(orderedResults(primaryResults), len(ordered), []Warning{partialWarning(c.primary, primaryErr)}), nil
		}
		return MultiResult{}, failedAttempt(c.primary, primaryErr)
	}
	fallbackResults, fallbackErr := transcribeBatchWith(ctx, c.fallback, ordered, c.options.Workers, progress)
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

func (c *Chain) finishMixedStream(ctx context.Context, segments []Segment, primary map[int]SegmentResult, primaryErr error, progress Progress) (MultiResult, error) {
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
	fallback, fallbackErr := transcribeBatchWith(ctx, c.fallback, failed, c.options.Workers, progress)
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
		return MultiResult{}, errs.New(errs.EmptyResponse, "stt", "aucun segment n'a produit de transcription")
	}
	return assemble(orderedResults(primary), len(segments), warnings), nil
}

type streamOutcome struct {
	segment Segment
	result  SegmentResult
	err     error
}

func transcribeStreamWith(ctx context.Context, provider Provider, input <-chan Segment, workers int, progress Progress) ([]Segment, map[int]SegmentResult, error) {
	jobs := make(chan Segment)
	var workerGroup sync.WaitGroup
	var mu sync.Mutex
	var outcomes []streamOutcome
	workerGroup.Add(workers)
	for range workers {
		go func() {
			defer workerGroup.Done()
			for segment := range jobs {
				outcome := transcribeOne(ctx, provider, segment, progress)
				mu.Lock()
				outcomes = append(outcomes, outcome)
				mu.Unlock()
			}
		}()
	}
	var segments []Segment
consume:
	for {
		select {
		case <-ctx.Done():
			break consume
		case segment, ok := <-input:
			if !ok {
				break consume
			}
			segments = append(segments, segment)
			select {
			case jobs <- segment:
			case <-ctx.Done():
				break consume
			}
		}
	}
	close(jobs)
	workerGroup.Wait()
	if err := ctx.Err(); err != nil {
		return segments, nil, err
	}
	return collectOutcomes(outcomes)
}

func transcribeBatchWith(ctx context.Context, provider Provider, segments []Segment, workers int, progress Progress) (map[int]SegmentResult, error) {
	input := make(chan Segment, len(segments))
	for _, segment := range segments {
		input <- segment
	}
	close(input)
	_, results, err := transcribeStreamWith(ctx, provider, input, workers, progress)
	return results, err
}

func transcribeOne(ctx context.Context, provider Provider, segment Segment, progress Progress) streamOutcome {
	if progress.Started != nil {
		progress.Started(segment.Index, provider.Name())
	}
	result, err := invoke(ctx, provider, segment.Request)
	if err != nil {
		if progress.Failed != nil {
			progress.Failed(segment.Index, provider.Name(), errs.Classify(err))
		}
		return streamOutcome{segment: segment, err: err}
	}
	if progress.Done != nil {
		progress.Done(segment.Index, result.Provider)
	}
	return streamOutcome{segment: segment, result: SegmentResult{Index: segment.Index, Text: result.Text, Provider: result.Provider}}
}

func collectOutcomes(outcomes []streamOutcome) ([]Segment, map[int]SegmentResult, error) {
	segments := make([]Segment, 0, len(outcomes))
	results := make(map[int]SegmentResult, len(outcomes))
	errorsByIndex := make(map[int]error)
	for _, outcome := range outcomes {
		segments = append(segments, outcome.segment)
		if outcome.err != nil {
			errorsByIndex[outcome.segment.Index] = outcome.err
		} else {
			results[outcome.segment.Index] = outcome.result
		}
	}
	ordered, err := validateSegments(segments)
	if err != nil {
		return segments, results, err
	}
	for _, segment := range ordered {
		if err := errorsByIndex[segment.Index]; err != nil {
			return segments, results, err
		}
	}
	return segments, results, nil
}

func orderedResults(results map[int]SegmentResult) []SegmentResult {
	ordered := make([]SegmentResult, 0, len(results))
	for _, result := range results {
		ordered = append(ordered, result)
	}
	for i := 1; i < len(ordered); i++ {
		current := ordered[i]
		j := i - 1
		for ; j >= 0 && ordered[j].Index > current.Index; j-- {
			ordered[j+1] = ordered[j]
		}
		ordered[j+1] = current
	}
	return ordered
}

func (c *Chain) transcribeMixed(ctx context.Context, segments []Segment) (MultiResult, error) {
	results := make([]SegmentResult, 0, len(segments))
	providers := make(map[string]struct{})
	partial := false
	var warnings []Warning
	fallbackUsed := false
	for _, segment := range segments {
		result, err := invoke(ctx, c.primary, segment.Request)
		if err != nil && c.fallback != nil && errs.Classify(err) != errs.Cancelled {
			primaryErr := err
			result, err = invoke(ctx, c.fallback, segment.Request)
			if err == nil && !fallbackUsed {
				warnings = append(warnings, fallbackWarning(c.primary, primaryErr))
				fallbackUsed = true
			} else if err != nil && (errs.Classify(err) == errs.Cancelled || !c.options.AllowPartial) {
				return MultiResult{}, failedAttempts(c.primary, c.fallback, primaryErr, err)
			}
		}
		if err != nil {
			if errs.Classify(err) == errs.Cancelled || !c.options.AllowPartial {
				return MultiResult{}, failedAttempt(c.primary, err)
			}
			partial = true
			continue
		}
		providers[result.Provider] = struct{}{}
		results = append(results, SegmentResult{Index: segment.Index, Text: result.Text, Provider: result.Provider})
	}
	if len(results) == 0 {
		return MultiResult{}, errs.New(errs.EmptyResponse, "stt", "aucun segment n'a produit de transcription")
	}
	if len(providers) > 1 {
		warnings = append(warnings, Warning{Class: errs.Unavailable, Component: "stt", Message: WarningMixedProviders})
	}
	if partial {
		warnings = append(warnings, Warning{Class: errs.ServerError, Component: "stt", Message: WarningPartial})
	}
	return assemble(results, len(segments), warnings), nil
}

func transcribeWith(ctx context.Context, provider Provider, segments []Segment, continueOnError bool) ([]SegmentResult, error) {
	results := make([]SegmentResult, 0, len(segments))
	var firstErr error
	for _, segment := range segments {
		result, err := invoke(ctx, provider, segment.Request)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if !continueOnError || errs.Classify(err) == errs.Cancelled {
				break
			}
			continue
		}
		results = append(results, SegmentResult{Index: segment.Index, Text: result.Text, Provider: result.Provider})
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
	for i := 1; i < len(ordered); i++ {
		current := ordered[i]
		j := i - 1
		for ; j >= 0 && ordered[j].Index > current.Index; j-- {
			ordered[j+1] = ordered[j]
		}
		ordered[j+1] = current
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Index == ordered[i].Index {
			return nil, errs.New(errs.ConfigInvalid, "stt", fmt.Sprintf("index de segment dupliqué: %d", ordered[i].Index))
		}
	}
	return ordered, nil
}

func assemble(results []SegmentResult, total int, warnings []Warning) MultiResult {
	parts := make([]string, 0, len(results))
	providers := make(map[string]struct{})
	for _, result := range results {
		if text := strings.TrimSpace(result.Text); text != "" {
			parts = append(parts, text)
		}
		providers[result.Provider] = struct{}{}
	}
	provider := ""
	if len(providers) == 1 {
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
	causes    []error
}

func (e *providerAttemptsError) Error() string   { return "échec d'un ou plusieurs providers STT" }
func (e *providerAttemptsError) Unwrap() []error { return e.causes }

func failedAttempt(provider Provider, err error) error {
	cause := &providerAttemptsError{effective: provider.Name(), causes: []error{err}}
	return errs.Wrap(errs.Classify(err), "stt", fmt.Sprintf("provider %q en échec (%s)", provider.Name(), errs.Classify(err)), cause)
}

func failedAttempts(primary, fallback Provider, primaryErr, fallbackErr error) error {
	cause := &providerAttemptsError{effective: fallback.Name(), causes: []error{primaryErr, fallbackErr}}
	message := fmt.Sprintf(
		"providers %q (%s) et %q (%s) en échec",
		primary.Name(), errs.Classify(primaryErr), fallback.Name(), errs.Classify(fallbackErr),
	)
	return errs.Wrap(errs.Classify(fallbackErr), "stt", message, cause)
}

func ProviderFromError(err error) string {
	var attempts *providerAttemptsError
	if errors.As(err, &attempts) {
		return attempts.effective
	}
	return ""
}
