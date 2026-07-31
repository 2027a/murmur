package logging

import (
	"bytes"
	"log"
	"testing"
)

func TestConfigureControlsWarnings(t *testing.T) {
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()
	var output bytes.Buffer
	log.SetOutput(&output)
	Configure("error")
	Warnf("hidden")
	if output.Len() != 0 {
		t.Fatalf("error level emitted warning: %q", output.String())
	}
	Configure("warn")
	Warnf("visible")
	if !bytes.Contains(output.Bytes(), []byte("warn: visible")) {
		t.Fatalf("warn level output = %q", output.String())
	}
}
