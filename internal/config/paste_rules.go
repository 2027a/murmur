package config

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validatePasteRules(sink SinkConfig) error {
	if len(sink.PasteRules) > 64 {
		return configError("sink.paste_rules accepte au plus 64 règles")
	}
	if len(sink.PasteRules) > 0 && sink.PasteBackend != "hyprland" {
		return configError("sink.paste_rules exige sink.paste_backend=hyprland")
	}
	for index, rule := range sink.PasteRules {
		class := rule.Class
		if class == "" || len(class) > 256 || strings.TrimSpace(class) != class ||
			!utf8.ValidString(class) || strings.IndexFunc(class, unicode.IsControl) != -1 {
			return configError(fmt.Sprintf("sink.paste_rules[%d].class doit être une classe non vide, sans contrôles ni espaces aux extrémités, de 256 octets maximum", index))
		}
		if !oneOf(rule.Shortcut, "ctrl+v", "ctrl+shift+v", "shift+insert") {
			return configError(fmt.Sprintf("sink.paste_rules[%d].shortcut contient un raccourci inconnu", index))
		}
	}
	return nil
}
