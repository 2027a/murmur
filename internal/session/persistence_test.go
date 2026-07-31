package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
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
		Warnings:   []string{"cleanup dégradé"},
	}
	if err := WriteLastResult(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLastResult(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != want.Result || got.Error.Class != errs.SinkFailed || got.Provider != want.Provider || len(got.Warnings) != 1 {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	info, err := os.Stat(LastResultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result permissions = %o", info.Mode().Perm())
	}
}
