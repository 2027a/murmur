package stt

import "testing"

func TestOverlapAssemblyPreservesFrenchBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, left, right, want string
		overlap                 bool
		gap                     int
		removed                 int
	}{
		{"proper name and punctuation", "Je vois Élodie à Saint-Étienne.", "À Saint-Étienne, demain.", "Je vois Élodie à Saint-Étienne. demain.", true, 1, 2},
		{"voice command", "commande stt titre bilan", "titre bilan de septembre fin du titre", "commande stt titre bilan de septembre fin du titre", true, 1, 2},
		{"negation", "Je ne veux pas", "veux pas supprimer le dossier", "Je ne veux pas supprimer le dossier", true, 1, 2},
		{"code switching", "une pull request", "pull request pour PipeWire", "une pull request pour PipeWire", true, 1, 2},
		{"numbers", "vingt trois euros", "trois euros et dix centimes", "vingt trois euros et dix centimes", true, 1, 2},
		{"legitimate repeated phrase", "répète très bien", "très bien, très bien.", "répète très bien très bien.", true, 1, 2},
		{"single repeated word", "non", "non, ne supprime pas", "non non, ne supprime pas", true, 1, 0},
		{"no acoustic overlap", "très bien", "très bien, continue", "très bien très bien, continue", false, 1, 0},
		{"partial gap", "très bien", "très bien, continue", "très bien très bien, continue", true, 2, 0},
		{"never erase entire segment", "très bien", "très bien", "très bien très bien", true, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := assemble([]SegmentResult{{Index: 0, Text: test.left, Provider: "local"}, {Index: test.gap, Text: test.right, Provider: "local", Overlap: test.overlap}}, 2, nil)
			if result.Text != test.want || result.Segments[1].OverlapWords != test.removed {
				t.Fatalf("result=%#v", result)
			}
			if result.Segments[1].Text != test.right {
				t.Fatal("original provider response changed")
			}
		})
	}
}
