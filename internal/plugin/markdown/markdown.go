package markdown

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/policy"
)

type Kind string

const (
	Text        Kind = "text"
	Heading     Kind = "heading"
	Bold        Kind = "bold"
	Italic      Kind = "italic"
	OrderedList Kind = "ordered_list"
	BulletList  Kind = "bullet_list"
)

type Segment struct {
	Kind  Kind     `json:"type"`
	Level int      `json:"level,omitempty"`
	Text  string   `json:"text,omitempty"`
	Items []string `json:"items,omitempty"`
}

type Document struct {
	Segments []Segment `json:"segments"`
}

type Plugin struct{}

func (Plugin) Name() string      { return "markdown" }
func (Plugin) Zone() policy.Zone { return policy.Local }

func (Plugin) Transform(_ context.Context, text string) (string, error) {
	if !triggerRE.MatchString(text) {
		return text, nil
	}
	document, err := Parse(text)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "plugin/markdown", "commande Markdown invalide", err)
	}
	rendered, err := document.Render()
	if err != nil {
		return "", errs.Wrap(errs.Internal, "plugin/markdown", "document Markdown invalide", err)
	}
	return rendered, nil
}

var (
	triggerRE      = regexp.MustCompile(`(?i)\bcommandes?\s+stt\b\s*[,;:]?\s*`)
	headingRE      = regexp.MustCompile(`(?i)^titre(?:\s+de)?\s+(?:niveau\s+)?(1|2|un|une|deux|premier|première|premiere|second|seconde|deuxième|deuxieme)\b`)
	boldRE         = regexp.MustCompile(`(?i)^gras\b`)
	italicRE       = regexp.MustCompile(`(?i)^italique\b`)
	bulletListRE   = regexp.MustCompile(`(?i)^liste\s+(?:à|a)\s+puces?\b`)
	orderedListRE  = regexp.MustCompile(`(?i)^liste\s+(?:num[eé]rot[eé]e?|num[eé]ro\s+(?:t|thé))\b`)
	headingEndRE   = regexp.MustCompile(`(?i)(?:\bcommandes?\s+stt\b\s*[,;:]?\s*)?\bfin\s+(?:du\s+|de\s+)?titre\b`)
	boldEndRE      = regexp.MustCompile(`(?i)(?:\bcommandes?\s+stt\b\s*[,;:]?\s*)?\bfin\s+(?:du\s+)?gras\b`)
	italicEndRE    = regexp.MustCompile(`(?i)(?:\bcommandes?\s+stt\b\s*[,;:]?\s*)?\bfin\s+(?:(?:de\s+)?l(?:['’]|\s)+)?(?:de\s+)?italique\b`)
	listEndRE      = regexp.MustCompile(`(?i)(?:\bcommandes?\s+stt\b\s*[,;:]?\s*)?\bfin\s+(?:de\s+)?liste\b`)
	bulletItemRE   = regexp.MustCompile(`(?i)\bitem\b\s*[,;:]?\s*`)
	orderedItemRE  = regexp.MustCompile(`(?i)\bitem(?:\s+(?:num[eé]ro\s*)?([1-9][0-9]*|un|une|deux|trois|quatre|cinq|six|sept|huit|neuf|dix))?\b\s*[,;:]?\s*`)
	numberPrefixRE = regexp.MustCompile(`(?i)^num[eé]ro\b`)
)

func Parse(source string) (Document, error) {
	var document Document
	position := 0
	for position < len(source) {
		location := triggerRE.FindStringIndex(source[position:])
		if location == nil {
			appendText(&document, source[position:])
			break
		}
		start := position + location[0]
		appendText(&document, source[position:start])
		afterTrigger := position + location[1]
		segment, next, err := parseCommand(source, afterTrigger)
		if err != nil {
			return Document{}, err
		}
		document.Segments = append(document.Segments, segment)
		position = next
	}
	if len(document.Segments) == 0 {
		appendText(&document, source)
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

func parseCommand(source string, position int) (Segment, int, error) {
	position = skipSeparators(source, position)
	rest := source[position:]
	if match := headingRE.FindStringSubmatchIndex(rest); match != nil {
		level, err := headingLevel(rest[match[2]:match[3]])
		if err != nil {
			return Segment{}, 0, err
		}
		contentStart := skipSeparators(source, position+match[1])
		content, next, err := parseBoundedPayload(source, contentStart, Heading, headingEndRE)
		if err != nil {
			return Segment{}, 0, err
		}
		next = skipBlockTerminator(source, next)
		return Segment{Kind: Heading, Level: level, Text: content}, next, nil
	}
	if match := boldRE.FindStringIndex(rest); match != nil {
		return parseInline(source, position+match[1], Bold, boldEndRE)
	}
	if match := italicRE.FindStringIndex(rest); match != nil {
		return parseInline(source, position+match[1], Italic, italicEndRE)
	}
	if match := bulletListRE.FindStringIndex(rest); match != nil {
		return parseList(source, position+match[1], BulletList)
	}
	if match := orderedListRE.FindStringIndex(rest); match != nil {
		return parseList(source, position+match[1], OrderedList)
	}
	return Segment{}, 0, errors.New("instruction inconnue après commande stt")
}

func parseInline(source string, contentStart int, kind Kind, endPattern *regexp.Regexp) (Segment, int, error) {
	contentStart = skipSeparators(source, contentStart)
	content, next, err := parseBoundedPayload(source, contentStart, kind, endPattern)
	if err != nil {
		return Segment{}, 0, err
	}
	return Segment{Kind: kind, Text: content}, next, nil
}

func parseBoundedPayload(source string, contentStart int, kind Kind, endPattern *regexp.Regexp) (string, int, error) {
	match := endPattern.FindStringIndex(source[contentStart:])
	if match == nil {
		return "", 0, fmt.Errorf("fermeture %s absente", kind)
	}
	content := cleanPayload(source[contentStart : contentStart+match[0]])
	if content == "" {
		return "", 0, fmt.Errorf("contenu %s vide", kind)
	}
	return content, contentStart + match[1], nil
}

func parseList(source string, contentStart int, kind Kind) (Segment, int, error) {
	contentStart = skipSeparators(source, contentStart)
	end := listEndRE.FindStringIndex(source[contentStart:])
	if end == nil {
		return Segment{}, 0, errors.New("fin de liste absente")
	}
	body := source[contentStart : contentStart+end[0]]
	markers, err := listMarkers(body, kind)
	if err != nil {
		return Segment{}, 0, err
	}
	if len(markers) == 0 || strings.Trim(strings.TrimSpace(body[:markers[0].start]), ",;:") != "" {
		return Segment{}, 0, errors.New("liste sans marqueur item initial")
	}
	items := make([]string, 0, len(markers))
	for i, marker := range markers {
		itemEnd := len(body)
		if i+1 < len(markers) {
			itemEnd = markers[i+1].start
		}
		item := cleanPayload(body[marker.end:itemEnd])
		if item == "" {
			return Segment{}, 0, errors.New("item de liste vide")
		}
		items = append(items, item)
	}
	next := skipBlockTerminator(source, contentStart+end[1])
	return Segment{Kind: kind, Items: items}, next, nil
}

type itemMarker struct {
	start    int
	end      int
	numbered bool
	number   int
}

func listMarkers(body string, kind Kind) ([]itemMarker, error) {
	if kind == BulletList {
		matches := bulletItemRE.FindAllStringIndex(body, -1)
		markers := make([]itemMarker, len(matches))
		for i, match := range matches {
			markers[i] = itemMarker{start: match[0], end: match[1]}
		}
		return markers, nil
	}

	matches := orderedItemRE.FindAllStringSubmatchIndex(body, -1)
	markers := make([]itemMarker, 0, len(matches))
	anyNumbered := false
	for _, match := range matches {
		marker := itemMarker{start: match[0], end: match[1]}
		if match[2] >= 0 {
			number, err := itemNumber(body[match[2]:match[3]])
			if err != nil {
				return nil, err
			}
			marker.numbered = true
			marker.number = number
			anyNumbered = true
		} else if numberPrefixRE.MatchString(strings.TrimLeft(body[marker.end:], " \t")) {
			return nil, errors.New("numéro d'item invalide")
		}
		markers = append(markers, marker)
	}
	if !anyNumbered {
		return markers, nil
	}
	for i, marker := range markers {
		if !marker.numbered {
			return nil, errors.New("marqueurs d'items numérotés et non numérotés mélangés")
		}
		if marker.number != i+1 {
			return nil, fmt.Errorf("numéro d'item %d, attendu %d", marker.number, i+1)
		}
	}
	return markers, nil
}

func itemNumber(value string) (int, error) {
	if number, err := strconv.Atoi(value); err == nil {
		return number, nil
	}
	switch strings.ToLower(value) {
	case "un", "une":
		return 1, nil
	case "deux":
		return 2, nil
	case "trois":
		return 3, nil
	case "quatre":
		return 4, nil
	case "cinq":
		return 5, nil
	case "six":
		return 6, nil
	case "sept":
		return 7, nil
	case "huit":
		return 8, nil
	case "neuf":
		return 9, nil
	case "dix":
		return 10, nil
	default:
		return 0, fmt.Errorf("numéro d'item invalide: %s", strconv.Quote(value))
	}
}

func skipBlockTerminator(source string, position int) int {
	if position < len(source) && strings.ContainsRune(".!?", rune(source[position])) {
		return position + 1
	}
	return position
}

func headingLevel(value string) (int, error) {
	switch strings.ToLower(value) {
	case "1", "un", "une", "premier", "première", "premiere":
		return 1, nil
	case "2", "deux", "second", "seconde", "deuxième", "deuxieme":
		return 2, nil
	default:
		return 0, fmt.Errorf("niveau de titre invalide: %s", strconv.Quote(value))
	}
}

func skipSeparators(text string, position int) int {
	for position < len(text) && strings.ContainsRune(" \t,;:", rune(text[position])) {
		position++
	}
	return position
}

func cleanPayload(text string) string {
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text), ",;"))
}

func appendText(document *Document, text string) {
	if text == "" {
		return
	}
	last := len(document.Segments) - 1
	if last >= 0 && document.Segments[last].Kind == Text {
		document.Segments[last].Text += text
		return
	}
	document.Segments = append(document.Segments, Segment{Kind: Text, Text: text})
}

func (document Document) Validate() error {
	if len(document.Segments) == 0 {
		return errors.New("document sans segments")
	}
	for _, segment := range document.Segments {
		switch segment.Kind {
		case Text:
			if segment.Level != 0 || len(segment.Items) != 0 {
				return errors.New("segment texte mal formé")
			}
			if err := validateContent(segment.Text, true); err != nil {
				return err
			}
		case Heading:
			if (segment.Level != 1 && segment.Level != 2) || len(segment.Items) != 0 {
				return errors.New("titre mal formé")
			}
			if err := validateContent(segment.Text, false); err != nil {
				return err
			}
		case Bold, Italic:
			if segment.Level != 0 || len(segment.Items) != 0 {
				return errors.New("segment inline mal formé")
			}
			if err := validateContent(segment.Text, false); err != nil {
				return err
			}
		case OrderedList, BulletList:
			if segment.Level != 0 || segment.Text != "" || len(segment.Items) == 0 {
				return errors.New("liste mal formée")
			}
			for _, item := range segment.Items {
				if err := validateContent(item, false); err != nil {
					return err
				}
			}
		default:
			return errors.New("type de segment inconnu")
		}
	}
	return nil
}

func validateContent(text string, allowEmpty bool) error {
	if !allowEmpty && strings.TrimSpace(text) == "" {
		return errors.New("contenu vide")
	}
	if triggerRE.MatchString(text) {
		return errors.New("commande vocale non consommée")
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "[[murmur_cmd:") || strings.Contains(lower, "__wq_md_") {
		return errors.New("token technique présent")
	}
	if !allowEmpty && strings.ContainsAny(text, "\r\n") {
		return errors.New("structure imbriquée interdite")
	}
	return nil
}

func (document Document) Render() (string, error) {
	if err := document.Validate(); err != nil {
		return "", err
	}
	var output string
	afterBlock := false
	for _, segment := range document.Segments {
		fragment, block := renderSegment(segment)
		if block {
			output = strings.TrimRight(output, " \t\r\n")
			if output != "" {
				output += "\n\n"
			}
			output += fragment + "\n\n"
			afterBlock = true
			continue
		}
		if afterBlock {
			fragment = strings.TrimLeft(fragment, " \t\r\n")
		}
		output += fragment
		afterBlock = false
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return "", errors.New("rendu vide")
	}
	if triggerRE.MatchString(output) || strings.Contains(strings.ToLower(output), "[[murmur_cmd:") {
		return "", errors.New("commande ou token présent dans le rendu")
	}
	return output, nil
}

func renderSegment(segment Segment) (string, bool) {
	switch segment.Kind {
	case Text:
		return segment.Text, false
	case Heading:
		return strings.Repeat("#", segment.Level) + " " + segment.Text, true
	case Bold:
		return "**" + segment.Text + "**", false
	case Italic:
		return "*" + segment.Text + "*", false
	case OrderedList:
		lines := make([]string, len(segment.Items))
		for i, item := range segment.Items {
			lines[i] = strconv.Itoa(i+1) + ". " + item
		}
		return strings.Join(lines, "\n"), true
	case BulletList:
		lines := make([]string, len(segment.Items))
		for i, item := range segment.Items {
			lines[i] = "- " + item
		}
		return strings.Join(lines, "\n"), true
	default:
		return "", false
	}
}
