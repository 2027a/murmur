package stt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/httpurl"
	"git.2027a.net/mathieu/murmur/internal/policy"
)

const MaxResponseBytes = 4 << 20

type OpenAICompatible struct {
	name           string
	endpoint       string
	model          string
	language       string
	prompt         string
	apiKey         string
	timeout        time.Duration
	healthEndpoint string
	healthEnabled  bool
	retries        int
	retryBackoff   time.Duration
	client         *http.Client
}

func NewOpenAICompatible(config Config) (*OpenAICompatible, error) {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errs.New(errs.ConfigInvalid, "stt", "base_url doit être une URL HTTP(S) absolue")
	}
	if config.AllowedZone == policy.Internet && parsed.Scheme != "https" && !config.AllowInsecure {
		return nil, errs.New(errs.ConfigInvalid, "stt", "base_url doit utiliser HTTPS en zone internet, sauf allow_insecure=true")
	}
	if strings.TrimSpace(config.Name) == "" {
		return nil, errs.New(errs.ConfigInvalid, "stt", "nom de provider manquant")
	}
	if config.Timeout <= 0 {
		return nil, errs.New(errs.ConfigInvalid, "stt", "timeout doit être strictement positif")
	}
	if config.Retries < 0 {
		return nil, errs.New(errs.ConfigInvalid, "stt", "le nombre de retries ne peut pas être négatif")
	}
	if config.RetryBackoff < 0 {
		return nil, errs.New(errs.ConfigInvalid, "stt", "le backoff ne peut pas être négatif")
	}
	if config.HealthEnabled && strings.TrimSpace(config.HealthEndpoint) == "" {
		return nil, errs.New(errs.ConfigInvalid, "stt", "health_endpoint est obligatoire lorsque health est activé")
	}
	healthEndpoint := ""
	if config.HealthEndpoint != "" {
		resolved, err := httpurl.ResolveEndpoint(parsed, config.HealthEndpoint)
		if err != nil {
			return nil, errs.Wrap(errs.ConfigInvalid, "stt", "health_endpoint invalide", err)
		}
		if config.AllowedZone == policy.Internet && resolved.Scheme != "https" && !config.AllowInsecure {
			return nil, errs.New(errs.ConfigInvalid, "stt", "health_endpoint doit utiliser HTTPS en zone internet, sauf allow_insecure=true")
		}
		if !httpurl.SameOrigin(parsed, resolved) {
			return nil, errs.New(errs.ConfigInvalid, "stt", "health_endpoint doit conserver la même origine que base_url")
		}
		healthEndpoint = resolved.String()
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errs.New(errs.ConfigInvalid, "stt", "modèle manquant")
	}

	transport := policy.TransportWithTrusted(config.AllowedZone, config.TrustedNetworks)
	endpoint, err := url.JoinPath(parsed.String(), "audio/transcriptions")
	if err != nil {
		return nil, errs.Wrap(errs.ConfigInvalid, "stt", "construction de l'endpoint impossible", err)
	}
	return &OpenAICompatible{
		name:           config.Name,
		endpoint:       endpoint,
		model:          config.Model,
		language:       config.Language,
		prompt:         config.Prompt,
		apiKey:         strings.TrimSpace(config.APIKey),
		timeout:        config.Timeout,
		healthEnabled:  config.HealthEnabled,
		healthEndpoint: healthEndpoint,
		retries:        config.Retries,
		retryBackoff:   config.RetryBackoff,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (p *OpenAICompatible) Name() string { return p.name }

func (p *OpenAICompatible) Transcribe(ctx context.Context, request Request) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt <= p.retries; attempt++ {
		if attempt > 0 {
			if err := waitBackoff(ctx, p.retryBackoff); err != nil {
				return Result{}, classifyTransportError("stt", err)
			}
		}
		result, safeRetry, err := p.transcribeOnce(ctx, request)
		if err == nil {
			return result, nil
		}
		lastErr = err
		class := errs.Classify(err)
		if ctx.Err() != nil || attempt == p.retries || !safeRetry || (class != errs.Unavailable && class != errs.ServerError && class != errs.Timeout) {
			break
		}
	}
	return Result{}, lastErr
}

func (p *OpenAICompatible) transcribeOnce(ctx context.Context, request Request) (Result, bool, error) {
	file, handle, err := request.Audio.OpenRIFF()
	if err != nil {
		return Result{}, false, errs.Wrap(errs.AudioFailed, "stt", "fichier audio inaccessible", err)
	}

	filename := request.Filename
	if filename == "" {
		filename = filepath.Base(request.Audio.Path())
	}
	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	var bodyBytes atomic.Int64
	body := &countingReadCloser{ReadCloser: pipeReader, count: &bodyBytes}
	writeDone := make(chan error, 1)
	go func() {
		writeErr := errors.Join(writeTranscriptionBody(multipartWriter, file, filename, request, p), handle.Close())
		if writeErr != nil {
			pipeWriter.CloseWithError(writeErr)
		} else {
			pipeWriter.Close()
		}
		writeDone <- writeErr
	}()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, body)
	if err != nil {
		body.Close()
		<-writeDone
		return Result{}, false, errs.Wrap(errs.Internal, "stt", "construction de la requête impossible", err)
	}
	httpRequest.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	if p.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	response, requestErr := p.client.Do(httpRequest)
	body.Close()
	<-writeDone
	if requestErr != nil {
		return Result{}, bodyBytes.Load() == 0, classifyTransportError("stt", requestErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		classified := classifyStatus("stt", response.StatusCode)
		safeRetry := (response.StatusCode >= 500 && response.StatusCode < 600) || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests
		drainAndClose(response.Body)
		return Result{}, safeRetry, classified
	}
	defer response.Body.Close()

	data, err := readBounded(response.Body)
	if err != nil {
		return Result{}, false, errs.Wrap(errs.ServerError, "stt", "lecture de la réponse impossible", err)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Result{}, false, errs.Wrap(errs.ServerError, "stt", "réponse JSON invalide", err)
	}
	payload.Text = strings.TrimSpace(payload.Text)
	if payload.Text == "" {
		return Result{}, false, errs.New(errs.EmptyResponse, "stt", "le provider n'a renvoyé aucun texte")
	}
	return Result{Text: payload.Text, Provider: p.name, Raw: json.RawMessage(data)}, false, nil
}

func (p *OpenAICompatible) Health(ctx context.Context) error {
	if !p.healthEnabled {
		return nil
	}
	if strings.TrimSpace(p.healthEndpoint) == "" {
		return errs.New(errs.ConfigInvalid, "stt-health", "endpoint de health manquant")
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.healthEndpoint, nil)
	if err != nil {
		return errs.Wrap(errs.ConfigInvalid, "stt-health", "endpoint de health invalide", err)
	}
	if p.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return classifyTransportError("stt-health", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		classified := classifyStatus("stt-health", response.StatusCode)
		drainAndClose(response.Body)
		return classified
	}
	defer response.Body.Close()
	if _, err := readBounded(response.Body); err != nil {
		return errs.Wrap(errs.ServerError, "stt-health", "lecture de la réponse impossible", err)
	}
	return nil
}

func writeTranscriptionBody(writer *multipart.Writer, file io.Reader, filename string, request Request, provider *OpenAICompatible) error {
	model := request.Model
	if model == "" {
		model = provider.model
	}
	language := request.Language
	if language == "" {
		language = provider.language
	}
	prompt := request.Prompt
	if prompt == "" {
		prompt = provider.prompt
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"model", model},
		{"language", language},
		{"prompt", prompt},
		{"response_format", "json"},
	} {
		if field.value != "" {
			if err := writer.WriteField(field.name, field.value); err != nil {
				return err
			}
		}
	}
	return writer.Close()
}

type countingReadCloser struct {
	io.ReadCloser
	count *atomic.Int64
}

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	r.count.Add(int64(n))
	return n, err
}

func readBounded(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBytes {
		return nil, fmt.Errorf("réponse supérieure à %d octets", MaxResponseBytes)
	}
	return data, nil
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, MaxResponseBytes))
	_ = body.Close()
}

func classifyStatus(component string, status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errs.New(errs.AuthFailed, component, fmt.Sprintf("authentification refusée (HTTP %d)", status))
	case status == http.StatusRequestTimeout:
		return errs.New(errs.Timeout, component, fmt.Sprintf("requête expirée (HTTP %d)", status))
	case status == http.StatusTooManyRequests:
		return errs.New(errs.Unavailable, component, fmt.Sprintf("provider temporairement indisponible (HTTP %d)", status))
	default:
		return errs.New(errs.ServerError, component, fmt.Sprintf("réponse HTTP inattendue: %d", status))
	}
}

func classifyTransportError(component string, err error) error {
	class := errs.Classify(err)
	if class == errs.PolicyBlocked || class == errs.Cancelled || class == errs.Timeout {
		return errs.Wrap(class, component, "appel interrompu", err)
	}
	return errs.Wrap(errs.Unavailable, component, "provider inaccessible", err)
}

func waitBackoff(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ Provider = (*OpenAICompatible)(nil)
var _ HealthChecker = (*OpenAICompatible)(nil)
var _ Provider = (*Chain)(nil)
