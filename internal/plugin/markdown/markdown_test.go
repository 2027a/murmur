package markdown

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	pluginruntime "git.2027a.net/mathieu/murmur/internal/plugin"
	"git.2027a.net/mathieu/murmur/internal/policy"
	"git.2027a.net/mathieu/murmur/internal/textpipe"
)

type fixture struct {
	Name     string `json:"name"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
	Fallback bool   `json:"fallback,omitempty"`
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	data, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []fixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestMarkdownFixtures(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, test := range fixtures {
		t.Run(test.Name, func(t *testing.T) {
			got, err := (Plugin{}).Transform(context.Background(), test.Input)
			if test.Fallback {
				if err == nil {
					t.Fatalf("Transform() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.Expected {
				t.Fatalf("Transform() = %q, want %q", got, test.Expected)
			}
			if triggerRE.MatchString(got) || strings.Contains(got, "MURMUR_CMD") {
				t.Fatalf("technical command leaked in %q", got)
			}
		})
	}
}

func TestInvalidFixturesFallBackWithoutTechnicalTokens(t *testing.T) {
	grammar, err := pluginruntime.New(Plugin{}, pluginruntime.Options{
		Timeout: time.Second, AllowedZones: []policy.Zone{policy.Local},
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := textpipe.New(textpipe.Options{CommandPack: "fr", Plugin: grammar})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range loadFixtures(t) {
		if !test.Fallback {
			continue
		}
		t.Run(test.Name, func(t *testing.T) {
			result, err := pipeline.Run(context.Background(), test.Input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != test.Expected {
				t.Fatalf("Run() = %q, want %q", result.Text, test.Expected)
			}
			if len(result.Warnings) != 1 || result.Warnings[0].Component != "plugin" {
				t.Fatalf("warnings = %#v", result.Warnings)
			}
			if strings.Contains(result.Text, "MURMUR_CMD") || strings.Contains(result.Text, "__WQ_") {
				t.Fatalf("technical token leaked in %q", result.Text)
			}
		})
	}
}

func TestMarkdownRequiresExplicitTrigger(t *testing.T) {
	for _, input := range []string{
		"titre niveau deux architecture",
		"liste à puces item un item deux fin de liste",
		"ce texte parle de gras et d'italique",
	} {
		got, err := (Plugin{}).Transform(context.Background(), input)
		if err != nil || got != input {
			t.Fatalf("Transform(%q) = %q, %v", input, got, err)
		}
	}
}

func TestDocumentValidationRejectsUnconsumedCommandsAndInvalidStructure(t *testing.T) {
	invalid := []Document{
		{Segments: []Segment{{Kind: Heading, Level: 3, Text: "titre"}}},
		{Segments: []Segment{{Kind: Text, Text: "commande stt titre 1 fuite"}}},
		{Segments: []Segment{{Kind: BulletList, Items: []string{""}}}},
		{Segments: []Segment{{Kind: Bold, Text: "[[MURMUR_CMD:token]]"}}},
		{Segments: manyTextSegments(maxDocumentSegments + 1)},
		{Segments: []Segment{{Kind: OrderedList, Items: numberedItems(maxListItems + 1)}}},
	}
	for _, document := range invalid {
		if err := document.Validate(); err == nil {
			t.Fatalf("Validate() accepted %#v", document)
		}
	}
}

func TestMarkdownRejectsOversizedOutputWithoutPartialText(t *testing.T) {
	document := Document{Segments: []Segment{{Kind: Text, Text: strings.Repeat("a", maxOutputBytes+1)}}}
	if got, err := document.Render(); err == nil || got != "" {
		t.Fatalf("Render() = %q, %v; want empty output and error", got, err)
	}
}

func TestMarkdownTransformHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := (Plugin{}).Transform(ctx, "commande stt titre 1 texte fin du titre")
	if !errors.Is(err, context.Canceled) || got != "" {
		t.Fatalf("Transform() = %q, %v; want cancellation without output", got, err)
	}
}

func TestMarkdownLimitFallsBackThroughPipelineWithoutPartialOutput(t *testing.T) {
	text := "commande stt liste à puces " + strings.Repeat("item trop long ", maxListItems+1) + "fin de liste"
	pipeline, err := textpipe.New(textpipe.Options{CommandPack: "fr", Plugin: Plugin{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != text {
		t.Fatalf("fallback text = %q, want original", result.Text)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Component != "plugin" || result.Warnings[0].Class != errs.Internal {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func manyTextSegments(count int) []Segment {
	segments := make([]Segment, count)
	for i := range segments {
		segments[i] = Segment{Kind: Text, Text: strconv.Itoa(i)}
	}
	return segments
}

func numberedItems(count int) []string {
	items := make([]string, count)
	for i := range items {
		items[i] = strconv.Itoa(i)
	}
	return items
}
