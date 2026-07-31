package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyAllClasses(t *testing.T) {
	for _, class := range Classes {
		t.Run(string(class), func(t *testing.T) {
			err := fmt.Errorf("outer: %w", Wrap(class, "test", "failure", errors.New("cause")))
			if got := Classify(err); got != class {
				t.Fatalf("Classify() = %q, want %q", got, class)
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Component != "test" {
				t.Fatalf("errors.As failed: %#v", typed)
			}
			if !errors.Is(err, &Error{Class: class}) {
				t.Fatal("errors.Is did not match class")
			}
		})
	}
}

func TestClassifyContextErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		want Class
	}{
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), Timeout},
		{fmt.Errorf("wrapped: %w", context.Canceled), Cancelled},
		{errors.New("plain"), Internal},
		{nil, ""},
	} {
		if got := Classify(test.err); got != test.want {
			t.Errorf("Classify(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestErrorMessageDoesNotExposeCause(t *testing.T) {
	err := Wrap(AuthFailed, "secret", "credential unavailable", errors.New("token-value"))
	if got := err.Error(); got != "secret: credential unavailable" {
		t.Fatalf("Error() = %q", got)
	}
}
