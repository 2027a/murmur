package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/policy"
)

const (
	tokenOpen  = "[[MURMUR_CMD:0123456789abcdef0123456789abcdef:0:open_quote]]"
	tokenClose = "[[MURMUR_CMD:0123456789abcdef0123456789abcdef:1:close_quote]]"
	tokenLine  = "[[MURMUR_CMD:0123456789abcdef0123456789abcdef:2:line_break]]"
)

func TestCleanupContractAndDefaultPrompt(t *testing.T) {
	input := "bonjour " + tokenOpen + " monde " + tokenClose
	for _, test := range []struct {
		name   string
		apiKey string
		auth   string
	}{
		{name: "without auth"},
		{name: "with auth", apiKey: "secret", auth: "Bearer secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != test.auth {
					t.Errorf("Authorization = %q, want %q", got, test.auth)
				}
				var payload struct {
					Model    string    `json:"model"`
					Messages []message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode: %v", err)
				}
				if payload.Model != "cleaner" || len(payload.Messages) != 2 || payload.Messages[0].Content != DefaultPrompt || payload.Messages[1].Content != input {
					t.Errorf("payload = %#v", payload)
				}
				json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Bonjour " + tokenOpen + " monde " + tokenClose + "."}}},
				})
			}))
			defer server.Close()
			client := newCleanupClient(t, Config{
				BaseURL: server.URL + "/v1", Model: "cleaner", APIKey: test.apiKey,
				Timeout: time.Second, AllowedZone: policy.Local,
			})
			result := client.Clean(context.Background(), input)
			if result.Text != "Bonjour "+tokenOpen+" monde "+tokenClose+"." || result.Degraded != nil || result.Warnings != nil {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCleanupRejectsAmbiguousBaseURL(t *testing.T) {
	for _, baseURL := range []string{
		"http://user:secret@127.0.0.1/v1",
		"http://127.0.0.1/v1?endpoint=/evil",
		"http://127.0.0.1/v1#fragment",
	} {
		if _, err := NewOpenAICompatible(Config{BaseURL: baseURL, Timeout: time.Second}); errs.Classify(err) != errs.ConfigInvalid {
			t.Errorf("base URL %q accepted: %v", baseURL, err)
		}
	}
}

func TestCleanupRejectsEmptyModel(t *testing.T) {
	_, err := NewOpenAICompatible(Config{BaseURL: "http://127.0.0.1/v1", Timeout: time.Second})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "modèle") {
		t.Fatalf("constructor error = %v", err)
	}
}

func TestCleanupRequiresHTTPSForInternetUnlessOptedIn(t *testing.T) {
	_, err := NewOpenAICompatible(Config{
		BaseURL: "http://example.com/v1", Model: "cleaner", Timeout: time.Second, AllowedZone: policy.Internet,
	})
	if errs.Classify(err) != errs.ConfigInvalid || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("constructor error = %v", err)
	}
	if _, err := NewOpenAICompatible(Config{
		BaseURL: "http://example.com/v1", Model: "cleaner", Timeout: time.Second, AllowedZone: policy.Internet, AllowInsecure: true,
	}); err != nil {
		t.Fatalf("constructor with allow_insecure=true: %v", err)
	}
}

func TestCleanupGuardsFallbackToInput(t *testing.T) {
	input := "bonjour " + tokenOpen + " monde " + tokenClose
	for _, test := range []struct {
		name   string
		output string
	}{
		{"empty", " "},
		{"meta", "Voici le texte corrigé: bonjour " + tokenOpen + " monde " + tokenClose},
		{"refusal", "Je ne peux pas corriger ce texte."},
		{"comment", "Remarque: texte ambigu."},
		{"missing sentinel", "Bonjour " + tokenOpen + " monde."},
		{"altered sentinel", "Bonjour " + tokenOpen + " monde [[MURMUR_CMD:0123456789abcdef0123456789abcdef:1:line_break]]."},
		{"reordered sentinel", "Bonjour " + tokenClose + " monde " + tokenOpen + "."},
		{"added sentinel", "Bonjour " + tokenOpen + " monde " + tokenClose + " " + tokenLine + "."},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := cleanupServer(t, test.output, http.StatusOK)
			defer server.Close()
			client := newCleanupClient(t, Config{BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local})
			result := client.Clean(context.Background(), input)
			if result.Text != input || !reflect.DeepEqual(result.Warnings, []string{WarningDegraded}) || errs.Classify(result.Degraded) != errs.EmptyResponse {
				t.Fatalf("result = %#v, class=%s", result, errs.Classify(result.Degraded))
			}
		})
	}
}

func TestCleanupHTTPTimeoutCancelEmptyLarge(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   errs.Class
	}{
		{"auth", http.StatusUnauthorized, `{}`, errs.AuthFailed},
		{"server", http.StatusInternalServerError, `{}`, errs.ServerError},
		{"invalid json", http.StatusOK, `{`, errs.ServerError},
		{"empty choices", http.StatusOK, `{"choices":[]}`, errs.EmptyResponse},
		{"large", http.StatusOK, strings.Repeat("x", maxResponseBytes+1), errs.ServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := newCleanupClient(t, Config{BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local})
			result := client.Clean(context.Background(), "input")
			if result.Text != "input" || errs.Classify(result.Degraded) != test.want {
				t.Fatalf("result=%#v class=%s", result, errs.Classify(result.Degraded))
			}
		})
	}

	blocking := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer blocking.Close()
	client := newCleanupClient(t, Config{BaseURL: blocking.URL, Timeout: 20 * time.Millisecond, AllowedZone: policy.Local})
	if result := client.Clean(context.Background(), "input"); errs.Classify(result.Degraded) != errs.Timeout {
		t.Fatalf("timeout result = %#v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client = newCleanupClient(t, Config{BaseURL: blocking.URL, Timeout: time.Second, AllowedZone: policy.Local})
	if result := client.Clean(ctx, "input"); errs.Classify(result.Degraded) != errs.Cancelled {
		t.Fatalf("cancel result = %#v", result)
	}
}

func TestCleanupNon2xxStatusPrecedesBodyErrors(t *testing.T) {
	largeBody := []byte(responseCanary + strings.Repeat("x", maxResponseBytes+1))
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
		{"over limit", maxResponseBytes, func() *trackedResponseBody {
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
				client := newCleanupClient(t, Config{
					BaseURL: "http://127.0.0.1", Timeout: time.Second, AllowedZone: policy.Local,
				})
				client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status.status, Header: make(http.Header), Body: body}, nil
				})

				result := client.Clean(context.Background(), "input")
				if got := errs.Classify(result.Degraded); got != status.want {
					t.Fatalf("class = %s, want %s: %v", got, status.want, result.Degraded)
				}
				if strings.Contains(result.Degraded.Error(), responseCanary) || strings.Contains(strings.Join(result.Warnings, " "), responseCanary) {
					t.Fatalf("response body leaked in result: %#v", result)
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

func TestCleanupRejectsRedirectAndDisablesEnvironmentProxy(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := newCleanupClient(t, Config{BaseURL: server.URL, Timeout: time.Second, AllowedZone: policy.Local})
	result := client.Clean(context.Background(), "input")
	if errs.Classify(result.Degraded) != errs.ServerError || redirected.Load() != 0 {
		t.Fatalf("redirect result=%#v target calls=%d", result, redirected.Load())
	}

	t.Setenv("HTTP_PROXY", target.URL)
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("cleanup transport uses an environment proxy: %#v", client.client.Transport)
	}
}

func cleanupServer(t *testing.T, output string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": output}}},
		})
	}))
}

func newCleanupClient(t *testing.T, config Config) *Client {
	t.Helper()
	if config.Model == "" {
		config.Model = "cleaner"
	}
	client, err := NewOpenAICompatible(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
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
