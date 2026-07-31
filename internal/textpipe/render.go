package textpipe

import (
	"errors"
	"strings"
	"unicode/utf8"
)

func Render(text string, protection Protection, pack *Pack, asciiQuotes bool) (string, error) {
	if pack == nil {
		return "", errors.New("command pack is nil")
	}
	if err := protection.Validate(text); err != nil {
		return "", err
	}
	if !protection.HasCommands() {
		return text, nil
	}

	chunks := make([]string, 0, len(protection.Tokens)+1)
	remaining := text
	for _, token := range protection.Tokens {
		at := strings.Index(remaining, token.Value)
		if at < 0 {
			return "", errors.New("protected command disappeared during rendering")
		}
		chunks = append(chunks, remaining[:at])
		remaining = remaining[at+len(token.Value):]
	}
	chunks = append(chunks, remaining)

	for i, token := range protection.Tokens {
		switch token.Intent {
		case OpenQuote:
			chunks[i] = trimTrailingPunctuation(chunks[i], ".,")
			chunks[i+1] = trimLeadingPunctuation(chunks[i+1], ".,")
		case CloseQuote:
			chunks[i] = trimTrailingPunctuation(chunks[i], ".,")
			chunks[i+1] = strings.TrimLeft(chunks[i+1], " \t")
		case LineBreak, ParagraphBreak:
			chunks[i] = trimTrailingPunctuation(strings.TrimRight(chunks[i], "\r\n"), ".,;:!?")
			chunks[i+1] = trimLeadingPunctuation(strings.TrimLeft(chunks[i+1], "\r\n"), ".,;:!?")
		default:
			return "", errors.New("protected command has an unknown intent")
		}
	}

	open, close := pack.quotes.Open, pack.quotes.Close
	if asciiQuotes {
		open, close = `" `, ` "`
	}
	var out strings.Builder
	out.Grow(len(text))
	for i, token := range protection.Tokens {
		out.WriteString(chunks[i])
		switch token.Intent {
		case OpenQuote:
			if chunks[i] != "" && !strings.HasSuffix(chunks[i], "\n") && !strings.HasSuffix(chunks[i], "\r") {
				out.WriteByte(' ')
			}
			out.WriteString(open)
		case CloseQuote:
			out.WriteString(close)
		case LineBreak:
			out.WriteByte('\n')
		case ParagraphBreak:
			out.WriteString("\n\n")
		}
	}
	out.WriteString(chunks[len(chunks)-1])
	result := out.String()
	for _, token := range protection.Tokens {
		if strings.Contains(result, token.Value) {
			return "", errors.New("protected command token leaked from rendering")
		}
	}
	return result, nil
}

func trimTrailingPunctuation(text, punctuation string) string {
	text = strings.TrimRight(text, " \t")
	if text == "" {
		return text
	}
	r, size := utf8.DecodeLastRuneInString(text)
	if strings.ContainsRune(punctuation, r) {
		text = strings.TrimRight(text[:len(text)-size], " \t")
	}
	return text
}

func trimLeadingPunctuation(text, punctuation string) string {
	text = strings.TrimLeft(text, " \t")
	if text == "" {
		return text
	}
	r, size := utf8.DecodeRuneInString(text)
	if strings.ContainsRune(punctuation, r) {
		text = strings.TrimLeft(text[size:], " \t")
	}
	return text
}
