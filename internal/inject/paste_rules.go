package inject

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PasteRule matches an exact window class. Classes are data, never executable
// code or regexes; the first matching rule wins.
type PasteRule struct {
	Class    string
	Shortcut string
}

const maxPasteRules = 64

func validPasteClass(class string) bool {
	return class != "" && len(class) <= 256 && strings.TrimSpace(class) == class &&
		utf8.ValidString(class) && strings.IndexFunc(class, unicode.IsControl) == -1
}

func validatePasteRules(rules []PasteRule) error {
	if len(rules) > maxPasteRules {
		return fmt.Errorf("au plus %d règles de collage sont acceptées", maxPasteRules)
	}
	for index, rule := range rules {
		if !validPasteClass(rule.Class) {
			return fmt.Errorf("règle %d: classe invalide", index)
		}
		if _, ok := shortcutArgs(rule.Shortcut); !ok {
			return fmt.Errorf("règle %d: raccourci inconnu", index)
		}
	}
	return nil
}

func (i *Injector) shortcutForClass(class string) string {
	for _, rule := range i.rules {
		if strings.EqualFold(rule.Class, class) {
			return rule.Shortcut
		}
	}
	return i.shortcut
}
