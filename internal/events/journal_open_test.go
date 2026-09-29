package events

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestJournalOpenDoesNotMutateRejectedTargets(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink", "fifo", "directory"} {
		for _, truncate := range []bool{false, true} {
			name := kind + "/append"
			if truncate {
				name = kind + "/truncate"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "session.jsonl")
				target := filepath.Join(dir, "unrelated")
				payload := []byte("unrelated file must survive\n")
				switch kind {
				case "hardlink", "symlink":
					if err := os.WriteFile(target, payload, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(target, 0o644); err != nil {
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
				case "fifo":
					target = path
					if err := syscall.Mkfifo(path, 0o640); err != nil {
						t.Fatal(err)
					}
					// A reader lets O_WRONLY reach descriptor validation instead
					// of failing earlier with ENXIO. Never read from the FIFO.
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
				flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
				if truncate {
					flags |= os.O_TRUNC
				}
				file, err := secureOpenJournalFile(path, flags)
				if err == nil {
					file.Close()
					t.Fatal("unsafe target accepted")
				}
				after, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() {
					t.Fatalf("rejected target changed: mode %o -> %o, size %d -> %d", before.Mode(), after.Mode(), before.Size(), after.Size())
				}
				if kind == "hardlink" || kind == "symlink" {
					got, err := os.ReadFile(target)
					if err != nil || !bytes.Equal(got, payload) {
						t.Fatalf("unrelated content changed: %q err=%v", got, err)
					}
				}
			})
		}
	}
}

func TestJournalOpenNormalizesAndTruncatesValidFile(t *testing.T) {
	for _, truncate := range []bool{false, true} {
		name := "append"
		if truncate {
			name = "truncate"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			initial := []byte("old content\n")
			if err := os.WriteFile(path, initial, 0o644); err != nil {
				t.Fatal(err)
			}
			flags := os.O_WRONLY | os.O_APPEND
			if truncate {
				flags |= os.O_TRUNC
			}
			file, err := secureOpenJournalFile(path, flags)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("unsafe accepted descriptor: %v", info.Mode())
			}
			if _, err := file.Write([]byte("new content\n")); err != nil {
				t.Fatal(err)
			}
			want := append(initial, []byte("new content\n")...)
			if truncate {
				want = []byte("new content\n")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("content=%q want=%q err=%v", got, want, err)
			}
		})
	}
}

func TestJournalSingleFileRotationRejectsReplacementWithoutMutation(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	target := filepath.Join(dir, "unrelated")
	payload := []byte("unrelated file must survive\n")
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	operations := defaultJournalOperations()
	open := operations.open
	calls := 0
	operations.open = func(path string, flags int) (*os.File, error) {
		calls++
		if calls == 2 {
			if err := os.Remove(path); err != nil {
				return nil, err
			}
			if err := os.Link(target, path); err != nil {
				return nil, err
			}
		}
		return open(path, flags)
	}
	journal, err := NewJournal(bus, path, JournalOptions{
		MaxBytes: journalLineSize(t) + 1, MaxFiles: 1, operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	bus.Publish(testEvent(StateChanged))
	bus.Publish(testEvent(StateChanged))
	assertJournalFailure(t, journal.Close(), JournalReopen)
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, payload) || before.Mode() != after.Mode() {
		t.Fatalf("rotation modified unrelated file: content=%q mode=%o err=%v", got, after.Mode(), err)
	}
}

func TestJournalStatValidationBeforeAndAfterChmod(t *testing.T) {
	valid := syscall.Stat_t{Mode: syscall.S_IFREG | 0o600, Uid: uint32(os.Geteuid()), Nlink: 1}
	for _, tc := range []struct {
		name   string
		change func(*syscall.Stat_t)
		unsafe bool
	}{
		{name: "regular owned single link"},
		{name: "foreign owner", change: func(s *syscall.Stat_t) { s.Uid++ }, unsafe: true},
		{name: "hardlink", change: func(s *syscall.Stat_t) { s.Nlink = 2 }, unsafe: true},
		{name: "unlinked", change: func(s *syscall.Stat_t) { s.Nlink = 0 }, unsafe: true},
		{name: "fifo", change: func(s *syscall.Stat_t) { s.Mode = syscall.S_IFIFO | 0o600 }, unsafe: true},
		{name: "directory", change: func(s *syscall.Stat_t) { s.Mode = syscall.S_IFDIR | 0o700 }, unsafe: true},
		{name: "permissions to normalize", change: func(s *syscall.Stat_t) { s.Mode = syscall.S_IFREG | 0o644 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stat := valid
			if tc.change != nil {
				tc.change(&stat)
			}
			for _, private := range []bool{false, true} {
				wantErr := tc.unsafe || (private && stat.Mode&0o777 != 0o600)
				if err := validateJournalStat(stat, private); (err != nil) != wantErr {
					t.Fatalf("private=%v error=%v, want error=%v", private, err, wantErr)
				}
			}
		})
	}
}

func TestJournalSingleFileRotationRemainsBounded(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	journal, err := NewJournal(bus, path, JournalOptions{MaxBytes: 512, MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		bus.Publish(testEvent(StateChanged))
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 512 || info.Mode().Perm() != 0o600 || readJournalEventCount(t, path) == 0 {
		t.Fatalf("invalid single-file journal: size=%d mode=%o", info.Size(), info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("unexpected rotation file: %v", err)
	}
}
