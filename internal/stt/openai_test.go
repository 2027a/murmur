package stt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/policy"
)

func TestOpenAICompatibleContract(t *testing.T) {
	audio := writeAudio(t, strings.Repeat("audio", 300000))
	for _, test := range []struct {
		name   string
		apiKey string
		auth   string
	}{
		{name: "speaches-like"},
		{name: "openai-like", apiKey: "secret", auth: "Bearer secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestProvider(t, Config{
				Name: "provider", BaseURL: "http://127.0.0.1/v1", Model: "large-v3", Language: "fr",
				Prompt: "vocabulaire", APIKey: test.apiKey, Timeout: 2 * time.Second, AllowedZone: policy.Local,
			})
			provider.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != test.auth {
					t.Errorf("Authorization = %q, want %q", got, test.auth)
				}
				if err := r.ParseMultipartForm(2 << 20); err != nil {
					t.Errorf("ParseMultipartForm: %v", err)
					return nil, err
				}
				for field, want := range map[string]string{
					"model": "large-v3", "language": "fr", "prompt": "vocabulaire", "response_format": "json",
				} {
					if got := r.FormValue(field); got != want {
						t.Errorf("%s = %q, want %q", field, got, want)
					}
				}
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Errorf("FormFile: %v", err)
					return nil, err
				}
				defer file.Close()
				data, err := io.ReadAll(file)
				if err != nil {
					t.Errorf("ReadAll: %v", err)
				}
				if header.Filename != "capture.wav" || len(data) != 1500044 {
					t.Errorf("file = %q, %d bytes", header.Filename, len(data))
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"text":" transcription "}`)),
				}, nil
			})
			result, err := provider.Transcribe(context.Background(), Request{Audio: audio, Filename: "capture.wav"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != "transcription" || result.Provider != "provider" || !json.Valid(result.Raw) {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestOpenAICompatibleUploadsOnlyPinnedValidatedRIFF(t *testing.T) {
	audio := writeAudio(t, "validated audio")
	expected, err := os.ReadFile(audio.Path())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := os.OpenFile(audio.Path(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString("CANARY-SUFFIX"); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	pinnedPath := audio.Path() + ".pinned"
	if err := os.Rename(audio.Path(), pinnedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio.Path(), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded = multipartFile(t, r)
		io.WriteString(w, `{"text":"ok"}`)
	}))
	defer server.Close()
	provider := newTestProvider(t, Config{Name: "test", BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local})
	if _, err := provider.Transcribe(context.Background(), Request{Audio: audio}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uploaded, expected) || bytes.Contains(uploaded, []byte("CANARY")) || bytes.Contains(uploaded, []byte("replacement")) {
		t.Fatalf("upload=%d octets, RIFF validé=%d", len(uploaded), len(expected))
	}
}

func TestOpenAICompatibleRejectsAmbiguousBaseURL(t *testing.T) {
	for _, baseURL := range []string{
		"http://user:secret@127.0.0.1/v1",
		"http://127.0.0.1/v1?endpoint=/evil",
		"http://127.0.0.1/v1#fragment",
	} {
		if _, err := NewOpenAICompatible(Config{Name: "test", BaseURL: baseURL, Timeout: time.Second}); errs.Classify(err) != errs.ConfigInvalid {
			t.Errorf("base URL %q accepted: %v", baseURL, err)
		}
	}
}

func TestOpenAICompatibleRejectsEmptyModel(t *testing.T) {
	_, err := NewOpenAICompatible(Config{Name: "test", BaseURL: "http://127.0.0.1/v1", Timeout: time.Second})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "modèle") {
		t.Fatalf("constructor error = %v", err)
	}
}

func TestOpenAICompatibleClassifiesResponses(t *testing.T) {
	audio := writeAudio(t, "audio")
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   errs.Class
	}{
		{"unauthorized", http.StatusUnauthorized, `{}`, errs.AuthFailed},
		{"forbidden", http.StatusForbidden, `{}`, errs.AuthFailed},
		{"request timeout", http.StatusRequestTimeout, `{}`, errs.Timeout},
		{"rate limited", http.StatusTooManyRequests, `{}`, errs.Unavailable},
		{"server error", http.StatusServiceUnavailable, `{}`, errs.ServerError},
		{"empty text", http.StatusOK, `{"text":" "}`, errs.EmptyResponse},
		{"missing text", http.StatusOK, `{}`, errs.EmptyResponse},
		{"invalid json", http.StatusOK, `{`, errs.ServerError},
		{"large response", http.StatusOK, strings.Repeat("x", MaxResponseBytes+1), errs.ServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			provider := newTestProvider(t, Config{Name: "test", BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local})
			_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
			if got := errs.Classify(err); got != test.want {
				t.Fatalf("error = %v, class = %s, want %s", err, got, test.want)
			}
		})
	}
}

func TestOpenAICompatibleNon2xxStatusPrecedesBodyErrors(t *testing.T) {
	audio := writeAudio(t, "audio")
	largeBody := []byte(responseCanary + strings.Repeat("x", MaxResponseBytes+1))
	statuses := []struct {
		name   string
		status int
		want   errs.Class
	}{
		{"unauthorized", http.StatusUnauthorized, errs.AuthFailed},
		{"forbidden", http.StatusForbidden, errs.AuthFailed},
		{"request timeout", http.StatusRequestTimeout, errs.Timeout},
		{"rate limited", http.StatusTooManyRequests, errs.Unavailable},
		{"server error", http.StatusServiceUnavailable, errs.ServerError},
	}
	bodies := []struct {
		name     string
		wantRead int64
		new      func() *trackedResponseBody
	}{
		{"over limit", MaxResponseBytes, func() *trackedResponseBody {
			return &trackedResponseBody{reader: bytes.NewReader(largeBody)}
		}},
		{"unreadable", 0, func() *trackedResponseBody {
			return &trackedResponseBody{reader: errorReader{err: errors.New(responseCanary)}}
		}},
	}

	for _, status := range statuses {
		for _, bodyTest := range bodies {
			t.Run(status.name+"/"+bodyTest.name, func(t *testing.T) {
				body := bodyTest.new()
				provider := newTestProvider(t, Config{
					Name: "test", BaseURL: "http://127.0.0.1", Timeout: time.Second, AllowedZone: policy.Local,
				})
				provider.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status.status, Header: make(http.Header), Body: body}, nil
				})

				_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
				if got := errs.Classify(err); got != status.want {
					t.Fatalf("class = %s, want %s: %v", got, status.want, err)
				}
				if strings.Contains(err.Error(), responseCanary) {
					t.Fatalf("response body leaked in error: %v", err)
				}
				if !body.closed {
					t.Fatal("response body was not closed")
				}
				if body.read != bodyTest.wantRead {
					t.Fatalf("drained %d bytes, want %d", body.read, bodyTest.wantRead)
				}
			})
		}
	}
}

func TestOpenAICompatibleTimeoutAndCancellation(t *testing.T) {
	audio := writeAudio(t, "audio")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()

	provider := newTestProvider(t, Config{Name: "test", BaseURL: server.URL, Timeout: 30 * time.Millisecond, AllowedZone: policy.Local})
	_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
	if got := errs.Classify(err); got != errs.Timeout {
		t.Fatalf("timeout class = %s, error = %v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider = newTestProvider(t, Config{Name: "test", BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local})
	_, err = provider.Transcribe(ctx, Request{Audio: audio})
	if got := errs.Classify(err); got != errs.Cancelled {
		t.Fatalf("cancel class = %s, error = %v", got, err)
	}
}

func TestOpenAICompatibleNetworkAndPolicyClassification(t *testing.T) {
	audio := writeAudio(t, "audio")
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := server.URL
	server.Close()
	provider := newTestProvider(t, Config{Name: "test", BaseURL: baseURL, Timeout: time.Second, AllowedZone: policy.Local})
	_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
	if got := errs.Classify(err); got != errs.Unavailable {
		t.Fatalf("network class = %s, error = %v", got, err)
	}

	provider = newTestProvider(t, Config{Name: "test", BaseURL: "http://203.0.113.1", Timeout: time.Second, AllowedZone: policy.Local})
	_, err = provider.Transcribe(context.Background(), Request{Audio: audio})
	if got := errs.Classify(err); got != errs.PolicyBlocked {
		t.Fatalf("policy class = %s, error = %v", got, err)
	}
}

func TestOpenAICompatibleRetryMatrix(t *testing.T) {
	audio := writeAudio(t, "audio")
	transportFailure := errors.New("transport failure")
	tests := []struct {
		name             string
		status           int
		requestBytesRead int
		transportErr     error
		responseBody     func() io.ReadCloser
		wantClass        errs.Class
		wantAttempts     int
	}{
		{name: "401", status: http.StatusUnauthorized, wantClass: errs.AuthFailed, wantAttempts: 1},
		{name: "408", status: http.StatusRequestTimeout, wantClass: errs.Timeout, wantAttempts: 3},
		{name: "429", status: http.StatusTooManyRequests, wantClass: errs.Unavailable, wantAttempts: 3},
		{name: "5xx", status: http.StatusServiceUnavailable, wantClass: errs.ServerError, wantAttempts: 3},
		{name: "6xx", status: 600, wantClass: errs.ServerError, wantAttempts: 1},
		{name: "transport before multipart bytes", transportErr: transportFailure, wantClass: errs.Unavailable, wantAttempts: 3},
		{name: "transport after multipart bytes", requestBytesRead: 1, transportErr: transportFailure, wantClass: errs.Unavailable, wantAttempts: 1},
		{
			name: "2xx response body read error", status: http.StatusOK,
			responseBody: func() io.ReadCloser {
				return io.NopCloser(errorReader{err: errors.New("response read failure")})
			},
			wantClass: errs.ServerError, wantAttempts: 1,
		},
		{
			name: "2xx invalid JSON", status: http.StatusOK,
			responseBody: func() io.ReadCloser {
				return io.NopCloser(strings.NewReader(`{`))
			},
			wantClass: errs.ServerError, wantAttempts: 1,
		},
		{
			name: "2xx oversized body", status: http.StatusOK,
			responseBody: func() io.ReadCloser {
				return io.NopCloser(strings.NewReader(strings.Repeat("x", MaxResponseBytes+1)))
			},
			wantClass: errs.ServerError, wantAttempts: 1,
		},
		{
			name: "2xx empty text", status: http.StatusOK,
			responseBody: func() io.ReadCloser {
				return io.NopCloser(strings.NewReader(`{"text":" "}`))
			},
			wantClass: errs.EmptyResponse, wantAttempts: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			provider := newTestProvider(t, Config{
				Name: "test", BaseURL: "http://127.0.0.1", Timeout: time.Second,
				AllowedZone: policy.Local, Retries: 2,
			})
			provider.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts++
				if test.requestBytesRead > 0 {
					buffer := make([]byte, test.requestBytesRead)
					if _, err := io.ReadFull(request.Body, buffer); err != nil {
						t.Fatalf("read multipart body: %v", err)
					}
				}
				if test.transportErr != nil {
					return nil, test.transportErr
				}
				body := io.ReadCloser(io.NopCloser(strings.NewReader(`{}`)))
				if test.responseBody != nil {
					body = test.responseBody()
				}
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: body}, nil
			})

			_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
			if got := errs.Classify(err); got != test.wantClass {
				t.Fatalf("class = %s, want %s: %v", got, test.wantClass, err)
			}
			if attempts != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, test.wantAttempts)
			}
		})
	}
}

func TestOpenAICompatibleTimeoutIncludesRetryBackoff(t *testing.T) {
	audio := writeAudio(t, "audio")
	var attempts atomic.Int32
	provider := newTestProvider(t, Config{
		Name: "test", BaseURL: "http://127.0.0.1", Timeout: 40 * time.Millisecond, AllowedZone: policy.Local,
		Retries: 4, RetryBackoff: 100 * time.Millisecond,
	})
	provider.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})
	started := time.Now()
	_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
	if errs.Classify(err) != errs.Timeout || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("global timeout: elapsed=%s error=%v", time.Since(started), err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts before global timeout = %d, want 1", got)
	}
}

func TestOpenAICompatibleHealthRedirectAndProxyPolicy(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			if r.Header.Get("Authorization") != "Bearer key" {
				t.Errorf("health auth = %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	provider := newTestProvider(t, Config{
		Name: "test", BaseURL: server.URL + "/v1", APIKey: "key", Timeout: time.Second,
		AllowedZone: policy.Local, HealthEnabled: true, HealthEndpoint: "/health",
	})
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	audio := writeAudio(t, "audio")
	_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
	if errs.Classify(err) != errs.ServerError {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect target was reached")
	}

	t.Setenv("HTTP_PROXY", target.URL)
	transport, ok := provider.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("provider transport uses an environment proxy: %#v", provider.client.Transport)
	}
}

func TestOpenAICompatibleHealthBearerStaysOnTLSOrigin(t *testing.T) {
	var originAuth atomic.Value
	var originRequests atomic.Int32
	originAuth.Store("")
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests.Add(1)
		originAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/health" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()

	var foreignAuth atomic.Value
	var foreignRequests atomic.Int32
	foreignAuth.Store("")
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		foreignAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer foreign.Close()

	_, err := NewOpenAICompatible(Config{
		Name: "test", BaseURL: origin.URL + "/v1", APIKey: "secret", Timeout: time.Second,
		AllowedZone: policy.Internet, HealthEnabled: true, HealthEndpoint: foreign.URL + "/health",
	})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "même origine") {
		t.Fatalf("cross-origin constructor error = %v", err)
	}
	if foreignRequests.Load() != 0 || foreignAuth.Load().(string) != "" {
		t.Fatalf("foreign health received %d requests with auth %q", foreignRequests.Load(), foreignAuth.Load())
	}

	provider := newTestProvider(t, Config{
		Name: "test", BaseURL: origin.URL + "/v1", APIKey: "secret", Timeout: time.Second,
		AllowedZone: policy.Internet, HealthEnabled: true, HealthEndpoint: "/health",
	})
	serverTransport := origin.Client().Transport.(*http.Transport)
	providerTransport := provider.client.Transport.(*http.Transport)
	providerTransport.TLSClientConfig = serverTransport.TLSClientConfig.Clone()
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if originRequests.Load() != 1 || originAuth.Load().(string) != "Bearer secret" {
		t.Fatalf("origin health received %d requests with auth %q", originRequests.Load(), originAuth.Load())
	}
}

func TestOpenAICompatibleRejectsInsecureInternetHealthEndpoint(t *testing.T) {
	_, err := NewOpenAICompatible(Config{
		Name: "test", BaseURL: "https://example.com/v1", Timeout: time.Second,
		AllowedZone: policy.Internet, HealthEnabled: true, HealthEndpoint: "http://example.com/health",
	})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("constructor error = %v", err)
	}
}

func newTestProvider(t *testing.T, config Config) *OpenAICompatible {
	t.Helper()
	if config.Model == "" {
		config.Model = "test-model"
	}
	provider, err := NewOpenAICompatible(config)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func writeAudio(t *testing.T, content string) wav.File {
	t.Helper()
	path := t.TempDir() + "/audio.wav"
	data := []byte(content)
	if len(data)%wav.BlockAlign != 0 {
		data = append(data, 0)
	}
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(data)))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], wav.Channels)
	binary.LittleEndian.PutUint32(header[24:28], wav.SampleRate)
	binary.LittleEndian.PutUint32(header[28:32], wav.ByteRate)
	binary.LittleEndian.PutUint16(header[32:34], wav.BlockAlign)
	binary.LittleEndian.PutUint16(header[34:36], wav.BitsPerSample)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(data)))
	if err := os.WriteFile(path, append(header, data...), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := wav.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func multipartFile(t *testing.T, request *http.Request) []byte {
	t.Helper()
	if err := request.ParseMultipartForm(2 << 20); err != nil {
		t.Fatal(err)
	}
	file, _, err := request.FormFile("file")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const responseCanary = "MURMUR_RESPONSE_BODY_CANARY"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackedResponseBody struct {
	reader io.Reader
	read   int64
	closed bool
}

func (b *trackedResponseBody) Read(buffer []byte) (int, error) {
	n, err := b.reader.Read(buffer)
	b.read += int64(n)
	return n, err
}

func (b *trackedResponseBody) Close() error {
	b.closed = true
	return nil
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
