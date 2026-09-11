package sysexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFakeExecutorRecordsDefensiveCopies(t *testing.T) {
	fake := &FakeExecutor{RunFunc: func(context.Context, string, []string, []byte) ([]byte, error) {
		return []byte("ok"), nil
	}}
	args := []string{"one"}
	stdin := []byte("input")
	stdout, err := fake.Run(context.Background(), "tool", args, stdin)
	if err != nil || string(stdout) != "ok" {
		t.Fatalf("Run() = %q, %v", stdout, err)
	}
	args[0] = "changed"
	stdin[0] = 'X'
	if got := fake.Calls(); !reflect.DeepEqual(got, []Call{{Name: "tool", Args: []string{"one"}, Stdin: []byte("input")}}) {
		t.Fatalf("Calls() = %#v", got)
	}
}

func TestFakeProcessLifecycle(t *testing.T) {
	p := NewFakeProcess()
	waited := make(chan error, 1)
	go func() { waited <- p.Wait() }()

	if err := p.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	want := errors.New("sortie")
	p.Exit(want)
	select {
	case err := <-waited:
		if !errors.Is(err, want) {
			t.Fatalf("Wait() = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait bloqué")
	}
	if got := p.Signals(); len(got) != 1 || got[0] != os.Interrupt {
		t.Fatalf("Signals() = %v", got)
	}
}

func TestOSExecutorRun(t *testing.T) {
	stdout, err := (OSExecutor{}).Run(context.Background(), "sh", []string{"-c", "read value; printf '%s' \"$value\""}, []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdout) != "hello" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestOSExecutorUsesExplicitEnvironment(t *testing.T) {
	t.Setenv("MURMUR_PARENT_SECRET", "must-not-be-inherited")
	stdout, err := (OSExecutor{Env: []string{"PATH=" + os.Getenv("PATH"), "SAFE=value"}}).Run(
		context.Background(), "sh", []string{"-c", `printf 'safe=%s secret=%s' "$SAFE" "$MURMUR_PARENT_SECRET"`}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(stdout); got != "safe=value secret=" {
		t.Fatalf("fake helper environment = %q", got)
	}
}

func TestOSExecutorRunLimitedBoundsHelperOutput(t *testing.T) {
	stdout, err := (OSExecutor{}).RunLimited(context.Background(), "sh", []string{"-c", "printf 12345"}, nil, 4)
	if !errors.Is(err, ErrOutputLimit) || stdout != nil {
		t.Fatalf("RunLimited() = %q, %v", stdout, err)
	}
}

func TestOSExecutorRunDiscardDoesNotBufferStdout(t *testing.T) {
	err := (OSExecutor{}).RunDiscard(context.Background(), "sh", []string{"-c", "yes ignored | head -c 1048576 >/dev/stdout"}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFilterEnvironmentRemovesEverySecretAssignment(t *testing.T) {
	filtered := FilterEnvironment([]string{"TOKEN=one", "SAFE=yes", "TOKEN=two", "OTHER=x"}, []string{"TOKEN", "OTHER"})
	if got := strings.Join(filtered, "|"); got != "SAFE=yes" {
		t.Fatalf("filtered environment = %q", got)
	}
}

func TestOSExecutorCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := (OSExecutor{}).Start(ctx, "sh", []string{"-c", "sleep 30 & wait"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("le groupe de processus a survécu à l'annulation")
	}
}

func TestOSExecutorRunCancellationKillsHelperGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (OSExecutor{}).Run(ctx, "sh", []string{"-c", "sleep 30 & child=$!; tmp=$1.tmp.$$; printf '%s' \"$child\" > \"$tmp\"; /bin/mv \"$tmp\" \"$1\"; wait", "sh", pidFile}, nil)
		done <- err
	}()
	var descendantPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			descendantPID, err = strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if descendantPID == 0 {
		t.Fatal("le descendant n'a pas démarré")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("le helper et son groupe ont survécu à l'annulation")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(descendantPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("le descendant %d a survécu à l'annulation", descendantPID)
}

func TestOSExecutorStartRejectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	process, err := (OSExecutor{}).Start(ctx, "sh", []string{"-c", "exit 0"})
	if !errors.Is(err, context.Canceled) || process != nil {
		t.Fatalf("Start() = %v, %v", process, err)
	}
}
