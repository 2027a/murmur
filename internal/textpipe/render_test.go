package textpipe

import (
	"context"
	"strings"
	"testing"
)

func TestClosingQuoteSeparators(t *testing.T) {
	for _, style := range []struct {
		name, pack, open, close, line, paragraph, quoted, again string
		ascii                                                   bool
	}{
		{name: "French", pack: "fr", open: "ouvre les guillemets", close: "ferme les guillemets", line: "à la ligne", paragraph: "nouveau paragraphe", quoted: "« bonjour »", again: "« encore »"},
		{name: "English", pack: "en", open: "open quotes", close: "close quotes", line: "new line", paragraph: "new paragraph", quoted: "“bonjour”", again: "“encore”"},
		{name: "French ASCII", pack: "fr", open: "ouvre les guillemets", close: "ferme les guillemets", line: "à la ligne", paragraph: "nouveau paragraphe", quoted: `" bonjour "`, again: `" encore "`, ascii: true},
		{name: "English ASCII", pack: "en", open: "open quotes", close: "close quotes", line: "new line", paragraph: "new paragraph", quoted: `" bonjour "`, again: `" encore "`, ascii: true},
	} {
		t.Run(style.name, func(t *testing.T) {
			pipeline, err := New(Options{CommandPack: style.pack, ASCIIQuotes: style.ascii})
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, suffix, want string
			}{
				{name: "following prose", suffix: " puis continuer", want: " puis continuer"},
				{name: "horizontal whitespace", suffix: "  \t puis continuer", want: " puis continuer"},
				{name: "comma", suffix: " , suite.", want: ", suite."},
				{name: "period", suffix: " .", want: "."},
				{name: "ellipsis", suffix: " …", want: "…"},
				{name: "closing parenthesis", suffix: " )", want: ")"},
				{name: "parenthetical prose", suffix: " (suite)", want: " (suite)"},
				{name: "literal newline", suffix: " \n suite", want: "\n suite"},
				{name: "line command", suffix: " " + style.line + " suite", want: "\nsuite"},
				{name: "paragraph command", suffix: " " + style.paragraph + " suite", want: "\n\nsuite"},
				{name: "next quote", suffix: " " + style.open + " encore " + style.close, want: " " + style.again},
				{name: "end of text"},
				{name: "trailing whitespace", suffix: " \t"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					input := style.open + " bonjour " + style.close + tc.suffix
					result, err := pipeline.Run(context.Background(), input)
					if err != nil {
						t.Fatal(err)
					}
					if want := style.quoted + tc.want; result.Text != want {
						t.Fatalf("result=%q, want %q", result.Text, want)
					}
				})
			}
		})
	}
}

func TestClosingQuoteSeparatesCleanupJoinedProse(t *testing.T) {
	pipeline, err := New(Options{CommandPack: "fr", Cleanup: CleanupFunc(func(_ context.Context, text string, protection Protection) (string, error) {
		last := protection.Tokens[len(protection.Tokens)-1].Value
		return strings.Replace(text, last+" ", last, 1), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background(), "ouvre les guillemets bonjour ferme les guillemets puis continuer")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "« bonjour » puis continuer" || len(result.Warnings) != 0 {
		t.Fatalf("result=%#v", result)
	}
}
