package textpipe

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxNamespaceAttempts = 128

type Token struct {
	Value  string `json:"value"`
	Intent Intent `json:"intent"`
}

// Protection is the cleanup-facing command conservation contract.
// Validate rejects missing, duplicated, reordered, altered, or added tokens.
type Protection struct {
	Namespace string  `json:"namespace"`
	Tokens    []Token `json:"tokens"`
}

func (p Protection) HasCommands() bool { return len(p.Tokens) != 0 }

func (p Protection) clone() Protection {
	p.Tokens = append([]Token(nil), p.Tokens...)
	return p
}

func (p Protection) Validate(text string) error {
	actual, err := p.tokensIn(text)
	if err != nil {
		return err
	}
	if len(actual) != len(p.Tokens) {
		return fmt.Errorf("protected command count changed: got %d, want %d", len(actual), len(p.Tokens))
	}
	for i, token := range p.Tokens {
		if actual[i] != token.Value {
			return fmt.Errorf("protected command %d changed", i+1)
		}
	}
	return nil
}

func (p Protection) tokensIn(text string) ([]string, error) {
	if p.Namespace == "" {
		if len(p.Tokens) == 0 {
			return nil, nil
		}
		return nil, errors.New("protected command namespace is empty")
	}
	var tokens []string
	remaining := text
	for {
		start := strings.Index(remaining, p.Namespace)
		if start < 0 {
			return tokens, nil
		}
		remaining = remaining[start:]
		end := strings.Index(remaining, "]]")
		if end < 0 {
			return nil, errors.New("protected command token is unterminated")
		}
		end += 2
		tokens = append(tokens, remaining[:end])
		remaining = remaining[end:]
	}
}

type Protected struct {
	Text       string     `json:"text"`
	Protection Protection `json:"protection"`
}

type Protector struct {
	random io.Reader
}

func NewProtector(random io.Reader) *Protector {
	return &Protector{random: random}
}

func Protect(text string, pack *Pack) (Protected, error) {
	return NewProtector(rand.Reader).Protect(text, pack)
}

func (p *Protector) Protect(text string, pack *Pack) (Protected, error) {
	if pack == nil {
		return Protected{}, errors.New("command pack is nil")
	}
	namespace, err := p.namespace(text)
	if err != nil {
		return Protected{}, err
	}
	matches := pack.matches(text)
	protection := Protection{Namespace: namespace, Tokens: make([]Token, 0, len(matches))}
	if len(matches) == 0 {
		return Protected{Text: text, Protection: protection}, nil
	}

	var out strings.Builder
	out.Grow(len(text) + len(matches)*len(namespace))
	last := 0
	for i, match := range matches {
		out.WriteString(text[last:match.start])
		value := namespace + strconv.Itoa(i) + ":" + string(match.intent) + "]]"
		out.WriteString(value)
		protection.Tokens = append(protection.Tokens, Token{Value: value, Intent: match.intent})
		last = match.end
	}
	out.WriteString(text[last:])
	return Protected{Text: out.String(), Protection: protection}, nil
}

func (p *Protector) namespace(text string) (string, error) {
	if p == nil || p.random == nil {
		return "", errors.New("sentinel random source is nil")
	}
	for range maxNamespaceAttempts {
		var nonce [16]byte
		if _, err := io.ReadFull(p.random, nonce[:]); err != nil {
			return "", fmt.Errorf("generate sentinel namespace: %w", err)
		}
		namespace := "[[MURMUR_CMD:" + hex.EncodeToString(nonce[:]) + ":"
		if !strings.Contains(text, namespace) {
			return namespace, nil
		}
	}
	return "", errors.New("could not generate a collision-free sentinel namespace")
}
