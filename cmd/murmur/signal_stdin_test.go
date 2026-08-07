package main

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestSignalStdinCancelReadTurnsBlockedPipeIntoEOF(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	stdin, err := newSignalStdin(reader)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := stdin.Read(make([]byte, 1))
		done <- err
	}()
	if err := stdin.CancelRead(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Read() error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lecture stdin encore bloquée")
	}
}
