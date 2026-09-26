package session

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/errs"
)

func TestConcurrentLockHasSingleWinner(t *testing.T) {
	dir := t.TempDir()
	results := make(chan bool, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock, err := AcquireLock(dir)
			if err != nil {
				if errs.Classify(err) != errs.Unavailable {
					t.Errorf("unexpected lock error: %v", err)
				}
				results <- false
				return
			}
			results <- true
			<-release
			if err := lock.Close(); err != nil {
				t.Errorf("close lock: %v", err)
			}
		}()
	}
	winners := 0
	for i := 0; i < 2; i++ {
		if <-results {
			winners++
		}
	}
	close(release)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
	info, err := os.Stat(dir + "/session.lock")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock permissions = %o", info.Mode().Perm())
	}
}

func TestLockDescriptorHandoff(t *testing.T) {
	dir := t.TempDir()
	parent, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	duplicateFD, err := syscall.Dup(int(parent.File().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	child, err := AdoptLock(os.NewFile(uintptr(duplicateFD), "inherited-session.lock"), filepath.Join(dir, "session.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Detach(); err != nil {
		t.Fatal(err)
	}
	if contender, err := AcquireLock(dir); err == nil {
		contender.Close()
		t.Fatal("lock was released during descriptor handoff")
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	contender, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("lock remains held after child close: %v", err)
	}
	contender.Close()
}

func TestAdoptLockAcquiresUnlockedDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := AdoptLock(file, path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	contender, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(contender.Fd()), syscall.LOCK_UN)
		t.Fatal("AdoptLock n'a pas acquis le verrou")
	}
}

func TestAdoptLockRejectsUnexpectedFile(t *testing.T) {
	dir := t.TempDir()
	expected := filepath.Join(dir, "session.lock")
	if err := os.WriteFile(expected, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.lock")
	file, err := os.OpenFile(other, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AdoptLock(file, expected); errs.Classify(err) != errs.Internal {
		t.Fatalf("AdoptLock() error = %v", err)
	}
}

func TestAdoptLockDoesNotLeakToExecDescendant(t *testing.T) {
	dir := t.TempDir()
	parent, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	duplicateFD, err := syscall.Dup(int(parent.File().Fd()))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := AdoptLock(os.NewFile(uintptr(duplicateFD), "inherited-session.lock"), filepath.Join(dir, "session.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Detach(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLockExecDescendant$")
	cmd.Env = append(os.Environ(), "MURMUR_LOCK_DESCENDANT=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	contender, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("exec descendant retained the adopted lock: %v", err)
	}
	_ = contender.Close()
}

func TestLockExecDescendant(t *testing.T) {
	if os.Getenv("MURMUR_LOCK_DESCENDANT") == "1" {
		time.Sleep(30 * time.Second)
	}
}

func TestAcquireLockRejectsSymlinkAndUnsafeFile(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "session.lock")); err != nil {
			t.Fatal(err)
		}
		if _, err := AcquireLock(dir); errs.Classify(err) != errs.Unavailable {
			t.Fatalf("AcquireLock() error = %v", err)
		}
	})

	t.Run("mode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.lock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := AcquireLock(dir); errs.Classify(err) != errs.Unavailable {
			t.Fatalf("AcquireLock() error = %v", err)
		}
	})
}

func TestLastResultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	want := LastResult{
		Version:    1,
		SessionID:  "session-42",
		Result:     Failed,
		Error:      &ResultError{Class: errs.SinkFailed, Component: "sink", Message: "livraison impossible"},
		Provider:   "speaches",
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Warnings: []ResultWarning{{
			Class: errs.Timeout, Provider: "speaches", Component: "cleanup", Message: "cleanup dégradé",
		}},
	}
	if err := MarkLastResultPending(dir, want.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := WriteLastResult(dir, want); err != nil {
		t.Fatal(err)
	}
	if err := CommitLastResult(dir, want.SessionID); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLastResult(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != want.Result || got.Error.Class != errs.SinkFailed || got.Provider != want.Provider ||
		len(got.Warnings) != 1 || got.Warnings[0].Class != errs.Timeout || got.Warnings[0].Component != "cleanup" {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	info, err := os.Stat(LastResultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result permissions = %o", info.Mode().Perm())
	}
	info, err = os.Stat(LastResultSessionPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result marker permissions = %o", info.Mode().Perm())
	}
}

func TestLastResultPendingMarkerRejectsOlderResult(t *testing.T) {
	dir := t.TempDir()
	old := testLastResult("old-session")
	if err := WriteLastResult(dir, old); err != nil {
		t.Fatal(err)
	}
	if err := MarkLastResultPending(dir, "new-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLastResult(dir); !errors.Is(err, ErrLastResultStale) {
		t.Fatalf("ReadLastResult() error = %v, want stale", err)
	}
	data, err := os.ReadFile(LastResultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"session_id": "old-session"`)) {
		t.Fatalf("old result was modified: %s", data)
	}
	assertNoLastResultTemporaryFiles(t, dir)
}

func TestLastResultPersistenceENOSPCNeverExposesOlderResult(t *testing.T) {
	for _, operation := range []string{"write", "rename"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			if err := WriteLastResult(dir, testLastResult("old-session")); err != nil {
				t.Fatal(err)
			}
			if err := MarkLastResultPending(dir, "new-session"); err != nil {
				t.Fatal(err)
			}
			writer := func(path string, _ []byte, _ os.FileMode) error {
				return &os.PathError{Op: operation, Path: path, Err: syscall.ENOSPC}
			}
			err := writeLastResultWith(dir, testLastResult("new-session"), writer)
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("WriteLastResult() error = %v, want ENOSPC", err)
			}
			if _, err := ReadLastResult(dir); !errors.Is(err, ErrLastResultStale) {
				t.Fatalf("ReadLastResult() error = %v, want stale", err)
			}
			assertNoLastResultTemporaryFiles(t, dir)
		})
	}
}

func TestLastResultMarkerENOSPCLeavesPreviousGenerationCurrent(t *testing.T) {
	for _, operation := range []string{"write", "rename"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			old := testLastResult("old-session")
			if err := MarkLastResultPending(dir, old.SessionID); err != nil {
				t.Fatal(err)
			}
			if err := WriteLastResult(dir, old); err != nil {
				t.Fatal(err)
			}
			if err := CommitLastResult(dir, old.SessionID); err != nil {
				t.Fatal(err)
			}
			writer := func(path string, _ []byte, _ os.FileMode) error {
				return &os.PathError{Op: operation, Path: path, Err: syscall.ENOSPC}
			}
			err := markLastResultPendingWith(dir, "new-session", writer)
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("MarkLastResultPending() error = %v, want ENOSPC", err)
			}
			got, err := ReadLastResult(dir)
			if err != nil || got.SessionID != old.SessionID {
				t.Fatalf("previous generation changed after rejected start: result=%#v error=%v", got, err)
			}
			assertNoLastResultTemporaryFiles(t, dir)
		})
	}
}

func TestLastResultCommitENOSPCLeavesNewResultPending(t *testing.T) {
	for _, operation := range []string{"write", "rename"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			if err := MarkLastResultPending(dir, "new-session"); err != nil {
				t.Fatal(err)
			}
			if err := WriteLastResult(dir, testLastResult("new-session")); err != nil {
				t.Fatal(err)
			}
			writer := func(path string, _ []byte, _ os.FileMode) error {
				return &os.PathError{Op: operation, Path: path, Err: syscall.ENOSPC}
			}
			err := commitLastResultWith(dir, "new-session", writer)
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("CommitLastResult() error = %v, want ENOSPC", err)
			}
			if _, err := ReadLastResult(dir); !errors.Is(err, ErrLastResultStale) {
				t.Fatalf("ReadLastResult() error = %v, want stale", err)
			}
			assertNoLastResultTemporaryFiles(t, dir)
		})
	}
}

func TestLastResultJournalRewriteIsCommittedOnlyAfterFinalWrite(t *testing.T) {
	dir := t.TempDir()
	result := testLastResult("journal-session")
	if err := MarkLastResultPending(dir, result.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := WriteLastResult(dir, result); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLastResult(dir); !errors.Is(err, ErrLastResultStale) {
		t.Fatalf("initial result became visible before runtime cleanup: %v", err)
	}
	AddJournalWarning(&result)
	if err := WriteLastResult(dir, result); err != nil {
		t.Fatal(err)
	}
	if err := CommitLastResult(dir, result.SessionID); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLastResult(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Component != "session-journal" {
		t.Fatalf("committed result=%#v", got)
	}
}

func TestLastResultValidateRejectsSemanticContradictions(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*LastResult)
	}{
		{"success with error", func(result *LastResult) {
			result.Error = &ResultError{Class: errs.Internal, Message: "unexpected"}
		}},
		{"failure without error", func(result *LastResult) {
			result.Result = Failed
			result.Error = nil
		}},
		{"error without class", func(result *LastResult) {
			result.Result = Failed
			result.Error = &ResultError{Message: "failed"}
		}},
		{"warning without component", func(result *LastResult) {
			result.Warnings = []ResultWarning{{Class: errs.Timeout, Message: "slow"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := testLastResult("semantic-session")
			test.mutate(&result)
			if err := result.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLastResultValidateKeepsProducedV1Shapes(t *testing.T) {
	for _, result := range []LastResult{
		testLastResult("completed-without-provider"),
		{
			Version: 1, SessionID: "cancelled", Result: Cancelled,
			StartedAt:  time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 7, 31, 10, 0, 1, 0, time.UTC),
		},
		{
			Version: 1, SessionID: "failed", Result: Failed,
			StartedAt:  time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 7, 31, 10, 0, 1, 0, time.UTC),
			Error:      &ResultError{Class: errs.Internal, Message: "échec de la session"},
			Warnings: []ResultWarning{{
				Class: errs.Internal, Component: "session-journal", Message: "journal de session dégradé",
			}},
		},
	} {
		if err := result.Validate(); err != nil {
			t.Fatalf("produced v1 result rejected: %#v: %v", result, err)
		}
	}
}

func testLastResult(sessionID string) LastResult {
	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	return LastResult{
		Version: 1, SessionID: sessionID, Result: Completed,
		StartedAt: now.Add(-time.Second), FinishedAt: now,
	}
}

func assertNoLastResultTemporaryFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".last-result") {
			t.Fatalf("partial result file left behind: %s", entry.Name())
		}
	}
}

func TestAddJournalWarningUsesOnlyControlledMetadata(t *testing.T) {
	result := LastResult{}
	AddJournalWarning(&result)
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings=%v", result.Warnings)
	}
	warning := result.Warnings[0]
	if warning.Class != errs.Internal || warning.Component != "session-journal" ||
		warning.Message != "journal de session dégradé" || warning.Provider != "" {
		t.Fatalf("warning=%#v", warning)
	}
}
