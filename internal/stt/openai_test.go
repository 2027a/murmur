package stt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/audio/wav"
	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/policy"
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != test.auth {
					t.Errorf("Authorization = %q, want %q", got, test.auth)
				}
				if err := r.ParseMultipartForm(2 << 20); err != nil {
					t.Errorf("ParseMultipartForm: %v", err)
					return
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
					return
				}
				defer file.Close()
				data, err := io.ReadAll(file)
				if err != nil {
					t.Errorf("ReadAll: %v", err)
				}
				if header.Filename != "capture.wav" || len(data) != 1500044 {
					t.Errorf("file = %q, %d bytes", header.Filename, len(data))
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"text":" transcription "}`)
			}))
			defer server.Close()

			provider := newTestProvider(t, Config{
				Name: "provider", BaseURL: server.URL + "/v1", Model: "large-v3", Language: "fr",
				Prompt: "vocabulaire", APIKey: test.apiKey, Timeout: 2 * time.Second, AllowedZone: policy.Local,
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
		{"large response", http.StatusOK, strings.Repeat("x", maxResponseBytes+1), errs.ServerError},
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

func TestOpenAICompatibleRetriesOnlyUnambiguousFailures(t *testing.T) {
	audio := writeAudio(t, strings.Repeat("audio", 1000))
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "later", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"text":"ok"}`)
	}))
	defer server.Close()
	provider := newTestProvider(t, Config{
		Name: "test", BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local, Retries: 2,
	})
	if _, err := provider.Transcribe(context.Background(), Request{Audio: audio}); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}

	attempts.Store(0)
	ambiguous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		io.Copy(io.Discard, r.Body)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter does not support hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer ambiguous.Close()
	provider = newTestProvider(t, Config{
		Name: "test", BaseURL: ambiguous.URL, Timeout: time.Second, AllowedZone: policy.Local, Retries: 3,
	})
	_, err := provider.Transcribe(context.Background(), Request{Audio: audio})
	if errs.Classify(err) != errs.Unavailable {
		t.Fatalf("ambiguous error = %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("ambiguous POST attempts = %d, want 1", got)
	}
}

func TestOpenAICompatibleDoesNotRetryAuthAndBoundsTotalTimeout(t *testing.T) {
	audio := writeAudio(t, "audio")
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	provider := newTestProvider(t, Config{
		Name: "test", BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local, Retries: 4,
	})
	provider.Transcribe(context.Background(), Request{Audio: audio})
	if got := attempts.Load(); got != 1 {
		t.Fatalf("auth attempts = %d, want 1", got)
	}

	attempts.Store(0)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	provider = newTestProvider(t, Config{
		Name: "test", BaseURL: server.URL, Timeout: 40 * time.Millisecond, AllowedZone: policy.Local,
		Retries: 4, RetryBackoff: 100 * time.Millisecond,
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

func newTestProvider(t *testing.T, config Config) *OpenAICompatible {
	t.Helper()
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
	return file
}
