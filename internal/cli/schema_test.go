package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestCommandResultFixtures(t *testing.T) {
	schema := compileCommandResultSchema(t)

	t.Run("valid", func(t *testing.T) {
		for _, path := range commandResultFixtures(t, "valid", "*.json") {
			t.Run(filepath.Base(path), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				validateCommandResultJSON(t, schema, data)
			})
		}
	})

	t.Run("invalid schema", func(t *testing.T) {
		for _, path := range commandResultFixtures(t, "invalid", "schema-*.json") {
			t.Run(filepath.Base(path), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(document); err == nil {
					t.Fatalf("fixture acceptée par le schéma: %s", data)
				}
			})
		}
	})

	t.Run("invalid semantics", func(t *testing.T) {
		for _, path := range commandResultFixtures(t, "invalid", "semantic-*.json") {
			t.Run(filepath.Base(path), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(document); err != nil {
					t.Fatalf("la fixture sémantique doit respecter le schéma: %v", err)
				}
				var result commandResult
				if err := json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				if err := result.validate(); err == nil {
					t.Fatalf("fixture acceptée par la validation sémantique: %s", data)
				}
			})
		}
	})
}

func TestRealComposableCommandResultsConformToContract(t *testing.T) {
	schema := compileCommandResultSchema(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "texte dicté"})
	}))
	defer server.Close()

	dir := privateCLITempDir(t)
	configPath := filepath.Join(dir, "config.toml")
	configBody := fmt.Sprintf(`[stt.providers.local]
type = "openai-compatible"
base_url = %q
model = "test-model"
language = "fr"
timeout_s = 5
zone = "local"
`, server.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidConfigPath := filepath.Join(dir, "invalid.toml")
	if err := os.WriteFile(invalidConfigPath, []byte("[stt]\nprovider = \"missing\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidWAV := filepath.Join(dir, "invalid.wav")
	if err := os.WriteFile(invalidWAV, []byte("not a wav"), 0o600); err != nil {
		t.Fatal(err)
	}

	tools := filepath.Join(dir, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLITestExecutable(t, filepath.Join(tools, "pw-record"), `#!/bin/sh
for output do :; done
cp "$MURMUR_TEST_WAV" "$output" || exit 1
trap 'exit 130' INT TERM
while :; do sleep 1; done
`)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MURMUR_TEST_WAV", cliWAV(t, time.Second))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "runtime"))
	if err := os.Mkdir(os.Getenv("XDG_RUNTIME_DIR"), 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		app      App
		args     []string
		wantCode int
	}{
		{name: "transcribe success", app: App{ConfigPath: configPath}, args: []string{"transcribe", "--json", cliWAV(t, time.Second)}},
		{name: "transcribe failure", app: App{ConfigPath: configPath}, args: []string{"transcribe", "--json", invalidWAV}, wantCode: 1},
		{name: "cleanup success", app: App{ConfigPath: configPath, Stdin: strings.NewReader("texte brut")}, args: []string{"cleanup", "--json"}},
		{name: "cleanup failure", app: App{ConfigPath: invalidConfigPath, Stdin: strings.NewReader("texte brut")}, args: []string{"cleanup", "--json"}, wantCode: 1},
		{name: "dictate omitted sink success", app: App{ConfigPath: configPath, Stdin: strings.NewReader("")}, args: []string{"dictate", "--json"}},
		{name: "dictate failure", app: App{ConfigPath: invalidConfigPath, Stdin: strings.NewReader("")}, args: []string{"dictate", "--sink", "stdout", "--json"}, wantCode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := test.app.Run(context.Background(), test.args, &stdout, &stderr)
			if code != test.wantCode || stderr.Len() != 0 {
				t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			result := validateCommandResultJSON(t, schema, stdout.Bytes())
			if result.Command != strings.Fields(test.name)[0] || result.Success != (test.wantCode == 0) {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

func compileCommandResultSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "docs", "cli-result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	schema, err := compiler.Compile(path)
	if err != nil {
		t.Fatalf("compilation du schéma Draft 2020-12: %v", err)
	}
	return schema
}

func commandResultFixtures(t *testing.T, class, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "cli-result", class, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("aucune fixture %s/%s", class, pattern)
	}
	return paths
}

func validateCommandResultJSON(t *testing.T, schema *jsonschema.Schema, data []byte) commandResult {
	t.Helper()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("JSON de résultat invalide: %v: %q", err, data)
	}
	if err := schema.Validate(document); err != nil {
		t.Fatalf("résultat hors schéma: %v\n%s", err, data)
	}
	var result commandResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if err := result.validate(); err != nil {
		t.Fatalf("résultat sémantiquement invalide: %v\n%s", err, data)
	}
	return result
}
