package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
	"git.2027a.net/mathieu/murmur/internal/policy"
)

type blockingPlugin struct{}

func (blockingPlugin) Name() string      { return "blocking" }
func (blockingPlugin) Zone() policy.Zone { return policy.Local }
func (blockingPlugin) Transform(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

type lanPlugin struct{ Plain }

func (lanPlugin) Zone() policy.Zone { return policy.LAN }

func TestPluginTimeoutIsRecoverable(t *testing.T) {
	bounded, err := New(blockingPlugin{}, Options{Timeout: 20 * time.Millisecond, AllowedZones: []policy.Zone{policy.Local}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = bounded.Transform(context.Background(), "secret transcript")
	if errs.Classify(err) != errs.Timeout || time.Since(started) > time.Second {
		t.Fatalf("Transform() error=%v duration=%v", err, time.Since(started))
	}
	if err != nil && errors.Is(err, context.Canceled) {
		t.Fatalf("timeout classified as cancellation: %v", err)
	}
}

func TestPluginZoneMustBeAllowed(t *testing.T) {
	_, err := New(lanPlugin{}, Options{Timeout: time.Second, AllowedZones: []policy.Zone{policy.Local}})
	if errs.Classify(err) != errs.PolicyBlocked {
		t.Fatalf("New() error = %v", err)
	}
}

func TestNonLocalPluginIsRejected(t *testing.T) {
	_, err := New(lanPlugin{}, Options{Timeout: time.Second, AllowedZones: []policy.Zone{policy.LAN}})
	if errs.Classify(err) != errs.ConfigInvalid {
		t.Fatalf("New() error = %v, class = %s", err, errs.Classify(err))
	}
}
