package clipboard

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"git.2027a.net/mathieu/murmur/internal/sysexec"
)

type call struct {
	name  string
	args  []string
	stdin string
}

type fakeExecutor struct {
	sysexec.OSExecutor
	mu       sync.Mutex
	calls    []call
	clearErr error
}

func (f *fakeExecutor) Run(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	if reflect.DeepEqual(args, []string{"--clear"}) {
		return nil, f.clearErr
	}
	return nil, nil
}

func TestModesRunExactCommands(t *testing.T) {
	tests := []struct {
		name string
		mode Mode
		want []call
	}{
		{name: "raw-and-clean", mode: RawAndClean, want: []call{
			{name: "wl-copy", stdin: "raw"},
			{name: "wl-copy", stdin: "clean"},
		}},
		{name: "clean-only", mode: CleanOnly, want: []call{{name: "wl-copy", stdin: "clean"}}},
		{name: "none", mode: None},
		{name: "transient", mode: Transient, want: []call{
			{name: "wl-copy", stdin: "clean"},
			{name: "wl-copy", args: []string{"--clear"}},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeExecutor{}
			clipboard, err := New(executor, test.mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := clipboard.CopyRaw(context.Background(), "raw"); err != nil {
				t.Fatal(err)
			}
			if err := clipboard.CopyFinal(context.Background(), "clean"); err != nil {
				t.Fatal(err)
			}
			clipboard.Clear(context.Background())
			if !reflect.DeepEqual(executor.calls, test.want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, test.want)
			}
		})
	}
}

func TestTransientClearIsBestEffort(t *testing.T) {
	executor := &fakeExecutor{clearErr: errors.New("selection owner gone")}
	clipboard, err := New(executor, Transient)
	if err != nil {
		t.Fatal(err)
	}
	clipboard.Clear(context.Background())
	if len(executor.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(executor.calls))
	}
}

func TestNoneDoesNotRequireExecutor(t *testing.T) {
	clipboard, err := New(nil, None)
	if err != nil {
		t.Fatal(err)
	}
	if err := clipboard.CopyFinal(context.Background(), "ignored"); err != nil {
		t.Fatal(err)
	}
}
