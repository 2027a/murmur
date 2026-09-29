package logging

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLogOpenRejectsFIFOWithoutReader(t *testing.T) {
	// Run the actual open in a subprocess: a regression that blocks in open(2)
	// is killed and reaped, rather than leaving a goroutine or FIFO reader behind.
	if path := os.Getenv("MURMUR_TEST_LOG_FIFO"); path != "" {
		var err error
		switch os.Getenv("MURMUR_TEST_LOG_ENTRY") {
		case "destination":
			var file *os.File
			file, err = OpenDestination(path)
			if file != nil {
				file.Close()
			}
		case "rotation":
			var writer *RotatingFile
			writer, err = NewRotatingFile(path, 128, 2)
			if writer != nil {
				writer.Close()
			}
		case "configure":
			err = ConfigureFile("info", path, 128, 2)
			defer Close()
		default:
			t.Fatal("unknown test entry point")
		}
		if err == nil {
			t.Fatal("FIFO accepted as a log file")
		}
		fmt.Println("FIFO rejected without a reader")
		return
	}
	for _, entry := range []string{"destination", "rotation", "configure"} {
		t.Run(entry, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "murmur.log")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLogOpenRejectsFIFOWithoutReader$")
			cmd.Env = append(os.Environ(),
				"MURMUR_TEST_LOG_FIFO="+path, "MURMUR_TEST_LOG_ENTRY="+entry,
				"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0",
			)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("log opening blocked on a FIFO with no reader")
			}
			if err != nil || !bytes.Contains(output, []byte("FIFO rejected without a reader")) {
				t.Fatalf("child error=%v output=%s", err, output)
			}
		})
	}
}

func TestLogOpenRejectsUnsafeTargetsWithoutMutation(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink", "fifo with reader", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "murmur.log")
			target := filepath.Join(dir, "unrelated")
			payload := []byte("do not touch unrelated data\n")
			switch kind {
			case "hardlink", "symlink":
				if err := os.WriteFile(target, payload, 0o644); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "hardlink" {
					err = os.Link(target, path)
				} else {
					err = os.Symlink(target, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "fifo with reader":
				target = path
				if err := syscall.Mkfifo(path, 0o640); err != nil {
					t.Fatal(err)
				}
				reader, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
			case "directory":
				target = path
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			file, err := OpenDestination(path)
			if err == nil {
				file.Close()
				t.Fatal("unsafe log target accepted")
			}
			after, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() {
				t.Fatalf("rejected target mutated: before=%v after=%v", before, after)
			}
			if kind == "hardlink" || kind == "symlink" {
				got, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("unrelated data changed: %q err=%v", got, err)
				}
			}
		})
	}
}

func TestLogOpenPreservesRegularContentAndSetsPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "murmur.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := OpenDestination(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("accepted log mode=%v", info.Mode())
	}
	if _, err := file.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "before\nafter\n" {
		t.Fatalf("log content=%q err=%v", got, err)
	}
}
