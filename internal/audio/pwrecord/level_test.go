package pwrecord

import (
	"context"
	"os"
	"testing"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio"
	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/sysexec"
)

func TestMeterUsesExistingCaptureAndStopsWithProcess(t *testing.T) {
	process := sysexec.NewFakeProcess()
	process.SignalFunc = func(os.Signal) error { process.Exit(nil); return nil }
	var path string
	executor := &sysexec.FakeExecutor{StartFunc: func(_ context.Context, _ string, args []string) (sysexec.Process, error) {
		path = args[len(args)-1]
		if err := wav.CreateGrowing(path); err != nil {
			t.Fatal(err)
		}
		if err := wav.AppendPCM(path, []byte{0, 64}); err != nil {
			t.Fatal(err)
		}
		return process, nil
	}}
	levels := make(chan wav.Level, 8)
	recording, err := New(executor).Start(context.Background(), audio.RecordOptions{
		Directory: t.TempDir(), Meter: func(l wav.Level) {
			select {
			case levels <- l:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer recording.Abort()
	if err := receiveReady(t, recording.Ready()); err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-levels:
		if l.RMS != .5 || l.UpdatedMS == 0 {
			t.Fatalf("level=%+v", l)
		}
	case <-time.After(time.Second):
		t.Fatal("no level")
	}
	select {
	case <-levels:
		t.Fatal("stale audio replayed")
	case <-time.After(100 * time.Millisecond):
	}
	if len(executor.Calls()) != 1 {
		t.Fatal("meter started another recorder")
	}
	if err := recording.Abort(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-levels:
		t.Fatal("level after abort")
	case <-time.After(100 * time.Millisecond):
	}
}
