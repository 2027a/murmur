package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLevelsHaveDistinctEffects(t *testing.T) {
	defer Close()
	for _, test := range []struct {
		level string
		want  []string
	}{
		{level: "debug", want: []string{"debug", "info", "warn", "error"}},
		{level: "info", want: []string{"info", "warn", "error"}},
		{level: "warn", want: []string{"warn", "error"}},
		{level: "error", want: []string{"error"}},
	} {
		t.Run(test.level, func(t *testing.T) {
			var output bytes.Buffer
			configureWriter(test.level, &output)
			Debugf(ComponentSession, "debug message")
			Infof(ComponentSession, "info message")
			Warnf(ComponentUI, "warn message")
			Errorf(ComponentInternal, "error message")

			var got []string
			scanner := bufio.NewScanner(&output)
			for scanner.Scan() {
				var value entry
				if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
					t.Fatal(err)
				}
				got = append(got, value.Level)
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("levels = %v, want %v", got, test.want)
			}
		})
	}
}

func TestUnknownComponentIsClosed(t *testing.T) {
	var output bytes.Buffer
	configureWriter("info", &output)
	Infof(Component("provider-name-or-secret"), "safe")
	var value entry
	if err := json.Unmarshal(output.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Component != ComponentInternal || bytes.Contains(output.Bytes(), []byte("provider-name-or-secret")) {
		t.Fatalf("entry = %s", output.Bytes())
	}
}

func TestRotatingFileIsBoundedAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "murmur.log")
	writer, err := NewRotatingFile(path, 128, 3)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 60)
	for i := 0; i < 10; i++ {
		if _, err := writer.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"murmur.log", "murmur.log.1", "murmur.log.2"} {
		info, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatalf("rotation %d: %v", index, err)
		}
		if info.Size() > 128 || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s size=%d mode=%o", name, info.Size(), info.Mode().Perm())
		}
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unexpected fourth file: %v", err)
	}
}

func TestWriteFailureIsBestEffort(t *testing.T) {
	configureWriter("debug", failingWriter{})
	Debugf(ComponentLogging, "write failure is not recursive")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("failure") }

func configureWriter(level string, writer interface{ Write([]byte) (int, error) }) {
	configured.Lock()
	configured.level = parseLevel(level)
	configured.output = writer
	configured.file = nil
	configured.Unlock()
}
