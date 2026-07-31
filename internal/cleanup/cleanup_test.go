package cleanup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/policy"
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
	client, err := NewOpenAICompatible(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
