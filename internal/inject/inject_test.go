package inject

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
	mu      sync.Mutex
	calls   []call
	results []error
}

func (f *fakeExecutor) Run(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	if len(f.results) == 0 {
		return nil, nil
	}
	err := f.results[0]
	f.results = f.results[1:]
	return nil, err
}

func TestPasteShortcutArguments(t *testing.T) {
	tests := []struct {
		shortcut string
		args     []string
	}{
		{"ctrl+shift+v", []string{"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"}},
		{"shift+insert", []string{"-M", "shift", "Insert", "-m", "shift"}},
		{"ctrl+v", []string{"-M", "ctrl", "v", "-m", "ctrl"}},
	}
	for _, test := range tests {
		t.Run(test.shortcut, func(t *testing.T) {
			executor := &fakeExecutor{}
			injector, err := New(executor, []string{test.shortcut}, Keep)
			if err != nil {
				t.Fatal(err)
			}
			if err := injector.Paste(context.Background(), "fallback"); err != nil {
				t.Fatal(err)
			}
			want := []call{{name: "wtype", args: test.args}}
			if !reflect.DeepEqual(executor.calls, want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, want)
			}
		})
	}
}

func TestPasteCascadeStopsAtFirstSuccess(t *testing.T) {
	executor := &fakeExecutor{results: []error{errors.New("unsupported"), nil}}
	injector, err := New(executor, []string{"ctrl+shift+v", "shift+insert", "ctrl+v"}, Keep)
	if err != nil {
		t.Fatal(err)
	}
	if err := injector.Paste(context.Background(), "unused"); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 || !reflect.DeepEqual(executor.calls[1].args, []string{"-M", "shift", "Insert", "-m", "shift"}) {
		t.Fatalf("calls = %#v", executor.calls)
	}
}

func TestPasteFallsBackToType(t *testing.T) {
	failure := errors.New("wtype shortcut failed")
	executor := &fakeExecutor{results: []error{failure, failure, failure, nil}}
	injector, err := New(executor, []string{"ctrl+shift+v", "shift+insert", "ctrl+v"}, Space)
	if err != nil {
		t.Fatal(err)
	}
	if err := injector.Paste(context.Background(), "one\ntwo"); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(executor.calls))
	}
	want := call{name: "wtype", args: []string{"-"}, stdin: "one two"}
	if !reflect.DeepEqual(executor.calls[3], want) {
		t.Fatalf("fallback = %#v, want %#v", executor.calls[3], want)
	}
}

func TestTypeNewlineModes(t *testing.T) {
	tests := []struct {
		mode NewlineMode
		want string
	}{
		{Keep, "one\r\ntwo\nthree"},
		{Space, "one two three"},
		{Marker, "one [NL] two [NL] three"},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			executor := &fakeExecutor{}
			injector, err := New(executor, nil, test.mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := injector.Type(context.Background(), "one\r\ntwo\nthree"); err != nil {
				t.Fatal(err)
			}
			want := []call{{name: "wtype", args: []string{"-"}, stdin: test.want}}
			if !reflect.DeepEqual(executor.calls, want) {
				t.Fatalf("calls = %#v, want %#v", executor.calls, want)
			}
		})
	}
}
