package core

import (
	"context"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/config"
)

func TestBuildProcessorUsesConfiguredASCIIQuotes(t *testing.T) {
	cfg := config.Defaults()
	cfg.General.CommandPack = "fr"
	cfg.General.ASCIIQuotes = true
	processor, err := BuildProcessor(context.Background(), cfg, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := processor.Run(context.Background(), "ouvre les guillemets salut ferme les guillemets")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `" salut "` {
		t.Fatalf("processed text = %q", result.Text)
	}
}
