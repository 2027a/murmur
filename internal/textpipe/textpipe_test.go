package textpipe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

type goldenCase struct {
	Name        string   `json:"name"`
	Pack        string   `json:"pack"`
	Input       string   `json:"input"`
	Intents     []Intent `json:"intents"`
	Expected    string   `json:"expected"`
	ASCIIQuotes bool     `json:"ascii_quotes"`
}

func TestGoldenCases(t *testing.T) {
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			pack, err := LoadPack(test.Pack, "")
			if err != nil {
				t.Fatal(err)
			}
			protected, err := Protect(test.Input, pack)
			if err != nil {
				t.Fatal(err)
			}
			var intents []Intent
			for _, token := range protected.Protection.Tokens {
				intents = append(intents, token.Intent)
			}
			if !reflect.DeepEqual(intents, test.Intents) {
				t.Fatalf("intents = %q, want %q", intents, test.Intents)
			}
			got, err := Render(protected.Text, protected.Protection, pack, test.ASCIIQuotes)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.Expected {
				t.Fatalf("Render() = %q, want %q", got, test.Expected)
			}
		})
	}
}

func TestPackParityAndSelection(t *testing.T) {
	fr, err := LoadPack("auto", "fr-FR")
	if err != nil {
		t.Fatal(err)
	}
	en, err := LoadPack("EN_us", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, pack := range []*Pack{fr, en} {
		for _, intent := range Intents {
			if len(pack.Variants(intent)) == 0 {
				t.Errorf("pack %s has no %s variants", pack.Language(), intent)
			}
		}
	}
	for _, test := range []struct {
		selection string
		language  string
	}{
		{"de", ""},
		{"auto", "de-DE"},
		{"auto", ""},
	} {
		if _, err := LoadPack(test.selection, test.language); errs.Classify(err) != errs.ConfigInvalid {
			t.Fatalf("LoadPack(%q, %q) error class = %q, want %q", test.selection, test.language, errs.Classify(err), errs.ConfigInvalid)
		}
	}
}

func TestAcceptance_19_PacksRemainIndependent(t *testing.T) {
	fr, err := LoadPack("fr", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := Protect("ouvrez les guillemets test fermez les guillemets", fr)
	if err != nil {
		t.Fatal(err)
	}
	beforeRendered, err := Render(before.Text, before.Protection, fr, false)
	if err != nil {
		t.Fatal(err)
	}

	data, err := packFiles.ReadFile("packs/en.json")
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"open quote",`), []byte(`"begin quotation", "open quote",`), 1)
	modifiedEnglish, err := ParsePack(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(modifiedEnglish.matches("begin quotation test")) != 1 {
		t.Fatal("modified English pack did not recognize its added variant")
	}

	after, err := Protect("ouvrez les guillemets test fermez les guillemets", fr)
	if err != nil {
		t.Fatal(err)
	}
	afterRendered, err := Render(after.Text, after.Protection, fr, false)
	if err != nil {
		t.Fatal(err)
	}
	if beforeRendered != afterRendered {
		t.Fatalf("French output changed after English extension: %q != %q", beforeRendered, afterRendered)
	}
}

func TestProtectionRetriesInputCollision(t *testing.T) {
	first := make([]byte, 16)
	second := bytes.Repeat([]byte{1}, 16)
	input := "literal [[MURMUR_CMD:00000000000000000000000000000000: text then new line end"
	pack, err := LoadPack("en", "")
	if err != nil {
		t.Fatal(err)
	}
	protected, err := NewProtector(bytes.NewReader(append(first, second...))).Protect(input, pack)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(protected.Protection.Namespace, strings.Repeat("0", 32)) {
		t.Fatal("protector reused a namespace colliding with input")
	}
	got, err := Render(protected.Text, protected.Protection, pack, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "literal [[MURMUR_CMD:00000000000000000000000000000000: text then\nend"
	if got != want {
		t.Fatalf("collision round trip = %q, want %q", got, want)
	}
}

func TestProtectionRejectsAlteredTokens(t *testing.T) {
	pack, err := LoadPack("en", "")
	if err != nil {
		t.Fatal(err)
	}
	protected, err := Protect("open quote test close quote", pack)
	if err != nil {
		t.Fatal(err)
	}
	first := protected.Protection.Tokens[0].Value
	second := protected.Protection.Tokens[1].Value
	for name, candidate := range map[string]string{
		"missing":    strings.Replace(protected.Text, first, "", 1),
		"altered":    strings.Replace(protected.Text, first, strings.Replace(first, "open_quote", "open_quot", 1), 1),
		"duplicated": protected.Text + first,
		"reordered":  second + " test " + first,
	} {
		t.Run(name, func(t *testing.T) {
			if err := protected.Protection.Validate(candidate); err == nil {
				t.Fatal("Validate() accepted altered protected commands")
			}
		})
	}
}

func TestGeneratedTokensNeverLeak(t *testing.T) {
	for _, language := range []string{"fr", "en"} {
		pack, err := LoadPack(language, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{
			"plain text",
			strings.Join(pack.Variants(OpenQuote), " "),
			strings.Join(pack.Variants(CloseQuote), " "),
			strings.Join(pack.Variants(LineBreak), " "),
			strings.Join(pack.Variants(ParagraphBreak), " "),
			".,;:!? mixed COMMANDS and punctuation",
		} {
			protected, err := Protect(input, pack)
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := Render(protected.Text, protected.Protection, pack, false)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(rendered, protected.Protection.Namespace) {
				t.Fatalf("generated namespace leaked for %s input %q", language, input)
			}
		}
	}
}

func TestCleanupCannotMutatePipelineProtection(t *testing.T) {
	pipeline, err := New(Options{
		CommandPack: "en",
		Cleanup: CleanupFunc(func(_ context.Context, text string, protection Protection) (string, error) {
			protection.Tokens[0].Value = "forged"
			return text, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background(), "open quote safe close quote")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "“safe”" || len(result.Warnings) != 0 {
		t.Fatalf("result after cleanup metadata mutation = %#v", result)
	}
}

func TestPipelineFallsBackOnAlteredCleanupToken(t *testing.T) {
	cleanupCalls := 0
	pluginCalls := 0
	pipeline, err := New(Options{
		CommandPack: "auto",
		STTLanguage: "en",
		Cleanup: CleanupFunc(func(_ context.Context, text string, protection Protection) (string, error) {
			cleanupCalls++
			return strings.Replace(text, protection.Tokens[0].Value, "altered", 1), nil
		}),
		Plugin: PluginFunc(func(_ context.Context, text string) (string, error) {
			pluginCalls++
			if strings.Contains(text, "MURMUR_CMD") {
				t.Fatal("plugin received a protected token")
			}
			return text + "!", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background(), "open quote one close quote new line two new paragraph three")
	if err != nil {
		t.Fatal(err)
	}
	if cleanupCalls != 1 || pluginCalls != 1 {
		t.Fatalf("calls = cleanup %d, plugin %d, want one each", cleanupCalls, pluginCalls)
	}
	if result.Text != "“one”\ntwo\n\nthree!" {
		t.Fatalf("Run() = %q", result.Text)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Class != errs.EmptyResponse || result.Warnings[0].Component != "cleanup" {
		t.Fatalf("warnings = %#v, want one cleanup warning", result.Warnings)
	}
}

func TestPlainModePreservesTextWithoutCommands(t *testing.T) {
	input := "  Odd,  spacing.\nAlready\n\nformatted; text!  "
	pipeline, err := New(Options{CommandPack: "fr"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != input {
		t.Fatalf("plain mode changed text: %q", result.Text)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("plain mode warnings = %#v", result.Warnings)
	}
}

func TestOptionalStageErrorsAreWarnings(t *testing.T) {
	pipeline, err := New(Options{
		CommandPack: "fr",
		Cleanup: CleanupFunc(func(_ context.Context, text string, _ Protection) (string, error) {
			return "", errors.New("cleanup unavailable")
		}),
		Plugin: PluginFunc(func(_ context.Context, text string) (string, error) {
			return "", errors.New("plugin unavailable")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background(), "ouvrez les guillemets texte fermez les guillemets")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "« texte »" {
		t.Fatalf("Run() = %q", result.Text)
	}
	if len(result.Warnings) != 2 || result.Warnings[0].Class != errs.Internal || result.Warnings[0].Component != "cleanup" ||
		result.Warnings[1].Class != errs.Internal || result.Warnings[1].Component != "plugin" {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestOptionalStageWarningsPreserveTypedClassesWithoutSensitiveErrors(t *testing.T) {
	const canary = "MURMUR-SENSITIVE-WARNING-CANARY"
	classes := []errs.Class{errs.ServerError, errs.Timeout, errs.AuthFailed, errs.PolicyBlocked, errs.Unavailable}
	for _, component := range []string{"cleanup", "plugin"} {
		for _, class := range classes {
			t.Run(component+"/"+string(class), func(t *testing.T) {
				stageErr := errs.Wrap(class, "unsafe/"+canary, "unsafe "+canary, errors.New("cause "+canary))
				options := Options{CommandPack: "fr"}
				if component == "cleanup" {
					options.Cleanup = CleanupFunc(func(context.Context, string, Protection) (string, error) {
						return "", stageErr
					})
				} else {
					options.Plugin = PluginFunc(func(context.Context, string) (string, error) {
						return "", stageErr
					})
				}
				pipeline, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(context.Background(), "texte sûr")
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Warnings) != 1 || result.Warnings[0].Class != class || result.Warnings[0].Component != component {
					t.Fatalf("warnings = %#v", result.Warnings)
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(encoded, []byte(canary)) {
					t.Fatalf("sensitive error leaked in %s", encoded)
				}
			})
		}
	}
}

func TestPipelineBudgetFallsBackForOptionalExpansion(t *testing.T) {
	tests := []struct {
		name      string
		options   Options
		want      string
		component string
	}{
		{
			name: "cleanup", component: "cleanup", want: "ok",
			options: Options{CommandPack: "fr", MaxTextBytes: 4, Cleanup: CleanupFunc(func(context.Context, string, Protection) (string, error) {
				return "beaucoup trop long", nil
			})},
		},
		{
			name: "plugin", component: "plugin", want: "ok",
			options: Options{CommandPack: "fr", MaxTextBytes: 4, Plugin: PluginFunc(func(context.Context, string) (string, error) {
				return "beaucoup trop long", nil
			})},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pipeline, err := New(test.options)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(context.Background(), "ok")
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != test.want || len(result.Warnings) != 1 || result.Warnings[0].Component != test.component || result.Warnings[0].Class != errs.ServerError {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

func TestPipelineBudgetUsesUTF8Bytes(t *testing.T) {
	pipeline, err := New(Options{CommandPack: "fr", MaxTextBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(context.Background(), "éé"); errs.Classify(err) != errs.ServerError {
		t.Fatalf("class=%q err=%v", errs.Classify(err), err)
	}
}

func TestPipelineHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pipeline, err := New(Options{CommandPack: "fr"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(ctx, "texte"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}
