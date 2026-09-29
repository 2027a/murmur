package stt

import (
	"strings"
	"unicode"
)

// matchingOverlap is intentionally opt-in and conservative. It only compares
// adjacent segments known to overlap acoustically, requires at least two words,
// and never erases an entire segment. It cannot disambiguate repeated phrases.
func matchingOverlap(previous, next string) int {
	left, right := boundaryWords(previous, 8, true), boundaryWords(next, 9, false)
	maximum := min(8, len(left), len(right)-1)
	for n := maximum; n >= 2; n-- {
		matched := true
		for i := range n {
			a, b := overlapWord(left[len(left)-n+i]), overlapWord(right[i])
			if a == "" || a != b {
				matched = false
				break
			}
		}
		if matched {
			return n
		}
	}
	return 0
}

func boundaryWords(text string, limit int, suffix bool) []string {
	words := make([]string, 0, limit)
	text = strings.TrimSpace(text)
	for text != "" && len(words) < limit {
		if suffix {
			start := strings.LastIndexFunc(text, unicode.IsSpace)
			if start < 0 {
				words = append(words, text)
				break
			}
			// Skip the complete UTF-8 whitespace rune, not one byte.
			words = append(words, strings.TrimSpace(text[start:]))
			text = strings.TrimRightFunc(text[:start], unicode.IsSpace)
		} else {
			end := strings.IndexFunc(text, unicode.IsSpace)
			if end < 0 {
				words = append(words, text)
				break
			}
			words = append(words, text[:end])
			text = strings.TrimLeftFunc(text[end:], unicode.IsSpace)
		}
	}
	if suffix {
		for i, j := 0, len(words)-1; i < j; i, j = i+1, j-1 {
			words[i], words[j] = words[j], words[i]
		}
	}
	return words
}

func overlapWord(word string) string {
	return strings.ToLower(strings.TrimFunc(word, unicode.IsPunct))
}

// TrimOverlap removes a known number of leading whitespace-delimited words,
// preserving the rest byte-for-byte. The count is exposed with each result so
// callers can reconstruct assembly without losing the original provider text.
func TrimOverlap(text string, words int) string {
	text = strings.TrimSpace(text)
	for words > 0 && text != "" {
		end := strings.IndexFunc(text, unicode.IsSpace)
		if end < 0 {
			return ""
		}
		text = strings.TrimLeftFunc(text[end:], unicode.IsSpace)
		words--
	}
	return text
}
