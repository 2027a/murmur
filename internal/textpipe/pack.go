package textpipe

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

//go:embed packs/*.json
var packFiles embed.FS

type quoteStyle struct {
	Open  string `json:"open"`
	Close string `json:"close"`
}

type packData struct {
	Language string              `json:"language"`
	Quotes   quoteStyle          `json:"quotes"`
	Variants map[Intent][]string `json:"variants"`
}

type commandPattern struct {
	intent Intent
	re     *regexp.Regexp
}

// Pack is an immutable set of language-specific formulations and typography.
type Pack struct {
	language string
	quotes   quoteStyle
	variants map[Intent][]string
	patterns []commandPattern
}

func (p *Pack) Language() string { return p.language }

func (p *Pack) Variants(intent Intent) []string {
	return append([]string(nil), p.variants[intent]...)
}

// LoadPack loads an embedded pack. "auto" follows the STT language.
func LoadPack(selection, sttLanguage string) (*Pack, error) {
	language := normalizeLanguage(selection)
	if language == "auto" {
		language = normalizeLanguage(sttLanguage)
	}
	if language == "" || language == "auto" {
		shown := language
		if shown == "" || shown == "auto" {
			shown = "unspecified"
		}
		return nil, unavailableLanguageError(shown)
	}

	data, err := packFiles.ReadFile("packs/" + language + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, unavailableLanguageError(language)
	}
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "textpipe", "read embedded command pack", err)
	}
	pack, err := ParsePack(data)
	if err != nil {
		return nil, err
	}
	if pack.language != language {
		return nil, errs.New(errs.Internal, "textpipe", "embedded command pack language mismatch")
	}
	return pack, nil
}

func unavailableLanguageError(language string) error {
	entries, err := packFiles.ReadDir("packs")
	if err != nil {
		return errs.Wrap(errs.Internal, "textpipe", "list embedded command packs", err)
	}
	available := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			available = append(available, strings.TrimSuffix(entry.Name(), ".json"))
		}
	}
	sort.Strings(available)
	return errs.New(errs.ConfigInvalid, "textpipe", fmt.Sprintf("command language %q is unavailable (available: %s)", language, strings.Join(available, ", ")))
}

// ParsePack validates pack data. It is exported so additional packs can be
// tested without changing the pipeline engine.
func ParsePack(data []byte) (*Pack, error) {
	var raw packData
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errs.Wrap(errs.ConfigInvalid, "textpipe", "decode command pack", err)
	}
	raw.Language = normalizeLanguage(raw.Language)
	if raw.Language == "" || raw.Language == "auto" {
		return nil, errs.New(errs.ConfigInvalid, "textpipe", "command pack has no concrete language")
	}
	if strings.TrimSpace(raw.Quotes.Open) == "" || strings.TrimSpace(raw.Quotes.Close) == "" {
		return nil, errs.New(errs.ConfigInvalid, "textpipe", "command pack has incomplete quote typography")
	}

	pack := &Pack{
		language: raw.Language,
		quotes:   raw.Quotes,
		variants: make(map[Intent][]string, len(Intents)),
	}
	seen := make(map[string]Intent)
	for _, intent := range Intents {
		variants := raw.Variants[intent]
		if len(variants) == 0 {
			return nil, errs.New(errs.ConfigInvalid, "textpipe", fmt.Sprintf("command pack has no variants for %q", intent))
		}
		for _, variant := range variants {
			variant = strings.Join(strings.Fields(variant), " ")
			if variant == "" {
				return nil, errs.New(errs.ConfigInvalid, "textpipe", fmt.Sprintf("command pack has an empty variant for %q", intent))
			}
			key := strings.ToLower(variant)
			if previous, ok := seen[key]; ok {
				return nil, errs.New(errs.ConfigInvalid, "textpipe", fmt.Sprintf("variant %q is ambiguous between %q and %q", variant, previous, intent))
			}
			seen[key] = intent
			pack.variants[intent] = append(pack.variants[intent], variant)
			words := strings.Fields(variant)
			for i := range words {
				words[i] = regexp.QuoteMeta(words[i])
			}
			pack.patterns = append(pack.patterns, commandPattern{
				intent: intent,
				re:     regexp.MustCompile(`(?i:` + strings.Join(words, `[[:space:]]+`) + `)`),
			})
		}
	}
	for intent := range raw.Variants {
		if !knownIntent(intent) {
			return nil, errs.New(errs.ConfigInvalid, "textpipe", fmt.Sprintf("command pack contains unknown intent %q", intent))
		}
	}
	return pack, nil
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(language, "_", "-")))
	if language == "auto" || language == "" {
		return language
	}
	if base, _, ok := strings.Cut(language, "-"); ok {
		return base
	}
	return language
}

func knownIntent(candidate Intent) bool {
	for _, intent := range Intents {
		if candidate == intent {
			return true
		}
	}
	return false
}

type commandMatch struct {
	start  int
	end    int
	intent Intent
}

func (p *Pack) matches(text string) []commandMatch {
	var candidates []commandMatch
	for _, pattern := range p.patterns {
		for _, location := range pattern.re.FindAllStringIndex(text, -1) {
			if commandBoundary(text, location[0], location[1]) {
				candidates = append(candidates, commandMatch{start: location[0], end: location[1], intent: pattern.intent})
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].end > candidates[j].end
	})

	matches := candidates[:0]
	lastEnd := -1
	for _, candidate := range candidates {
		if candidate.start < lastEnd {
			continue
		}
		matches = append(matches, candidate)
		lastEnd = candidate.end
	}
	return matches
}

func commandBoundary(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' {
			return false
		}
	}
	return true
}
