package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(help) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage: murmur") || stderr.Len() != 0 {
		t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"wat"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(wat) = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "commande inconnue: wat") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
