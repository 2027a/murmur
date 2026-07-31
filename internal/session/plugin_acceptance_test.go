package session

import (
	"context"
	"strings"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/config"
	"git.2027a.net/mathieu/murmur/internal/textpipe"
)

func TestAcceptance_15_MarkdownDisabledPreservesOrdinaryWords(t *testing.T) {
	cfg := config.Defaults()
	cfg.Plugin.Mode = "markdown"
	grammar, err := buildPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if grammar != nil {
		t.Fatal("markdown plugin enabled by default")
	}
	pipeline, err := textpipe.New(textpipe.Options{CommandPack: "fr", Plugin: grammar})
	if err != nil {
		t.Fatal(err)
	}
	input := "Le titre de cette liste reste du texte ordinaire."
	result, err := pipeline.Run(context.Background(), input)
	if err != nil || result.Text != input || len(result.Warnings) != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestAcceptance_16_InvalidMarkdownFallsBackWithoutTechnicalTokens(t *testing.T) {
	cfg := config.Defaults()
	cfg.Plugin.Enabled = true
	cfg.Plugin.Mode = "markdown"
	grammar, err := buildPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := textpipe.New(textpipe.Options{CommandPack: "fr", Plugin: grammar})
	if err != nil {
		t.Fatal(err)
	}
	cleaned := "Avant commande stt gras fermeture absente"
	result, err := pipeline.Run(context.Background(), cleaned)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != cleaned || len(result.Warnings) != 1 || result.Warnings[0].Component != "plugin" {
		t.Fatalf("Run() = %#v", result)
	}
	if strings.Contains(result.Text, "MURMUR_CMD") || strings.Contains(result.Text, "__WQ_") {
		t.Fatalf("technical token leaked in %q", result.Text)
	}
}
